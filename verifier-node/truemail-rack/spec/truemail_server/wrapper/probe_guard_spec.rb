# frozen_string_literal: true

# FORK EDIT (ADR-0021): the SMTP probe's destination guard. Every non-public range is refused, each with its public NEIGHBOURS as
# controls (an off-by-one range edge is the classic way a guard like this is wrong in only one direction), and the tests never touch
# the network: the addresses are given, not resolved. The run against Ruby's resolver and an independent oracle is
# scripts/check-probe-guard.sh.
RSpec.describe TruemailServer::Wrapper::ProbeGuard do
  # address => the range label it must be refused under
  blocked = {
    '0.0.0.0' => 'this-network', '0.255.255.255' => 'this-network',
    '10.0.0.0' => 'private-rfc1918', '10.1.2.3' => 'private-rfc1918', '10.255.255.255' => 'private-rfc1918',
    '100.64.0.0' => 'cgnat', '100.100.100.200' => 'cgnat', '100.127.255.255' => 'cgnat',
    '127.0.0.1' => 'loopback', '127.255.255.254' => 'loopback',
    '169.254.0.0' => 'link-local/cloud-metadata', '169.254.169.254' => 'link-local/cloud-metadata',
    '169.254.255.255' => 'link-local/cloud-metadata',
    '172.16.0.0' => 'private-rfc1918', '172.31.255.255' => 'private-rfc1918',
    '192.0.0.0' => 'ietf-protocol-assignments', '192.0.0.192' => 'ietf-protocol-assignments',
    '192.0.2.1' => 'documentation-test-net-1', '192.88.99.1' => '6to4-relay-anycast',
    '192.168.0.1' => 'private-rfc1918', '192.168.255.255' => 'private-rfc1918',
    '198.18.0.0' => 'benchmarking', '198.19.255.255' => 'benchmarking',
    '198.51.100.7' => 'documentation-test-net-2', '203.0.113.9' => 'documentation-test-net-3',
    '224.0.0.1' => 'multicast', '239.255.255.255' => 'multicast',
    '240.0.0.1' => 'reserved/broadcast', '255.255.255.255' => 'reserved/broadcast',
    # IPv6
    '::' => 'unspecified', '::1' => 'loopback', '::2' => 'ipv4-compatible',
    '64:ff9b::808:808' => 'nat64', '100::1' => 'discard-only', '5f00::1' => 'srv6-sids',
    'fc00::1' => 'ula-private', 'fd00:ec2::254' => 'ula-private', 'fe80::1' => 'link-local', 'fec0::1' => 'site-local',
    'ff02::1' => 'multicast', '2001::1' => 'ietf-protocol-assignments/teredo', '2001:db8::1' => 'documentation',
    '2002:808:808::1' => '6to4', '3fff::1' => 'documentation',
    # an IPv4-mapped address is judged as the IPv4 address it will actually connect to
    '::ffff:127.0.0.1' => 'loopback', '::ffff:10.0.0.1' => 'private-rfc1918',
    '::ffff:169.254.169.254' => 'link-local/cloud-metadata'
  }.freeze

  # the neighbours of every IPv4 edge above, plus ordinary public addresses
  public_addresses = %w[
    1.1.1.1 8.8.8.8 93.184.216.34 11.0.0.1 126.255.255.255 128.0.0.1
    100.63.255.255 100.128.0.0 169.253.255.255 169.255.0.0 172.15.255.255 172.32.0.0
    192.0.1.1 192.167.255.255 192.169.0.1 198.17.255.255 198.20.0.0 223.255.255.255
    2606:4700:4700::1111 2a00:1450:4001:81b::200e 2001:4860:4860::8888 2400:cb00::1 ::ffff:8.8.8.8
  ].freeze

  describe '.non_public_range' do
    blocked.each do |address, label|
      it "refuses #{address} as #{label}" do
        expect(described_class.non_public_range(address)).to eq(label)
      end
    end

    public_addresses.each do |address|
      it "lets the public address #{address} proceed (the control)" do
        expect(described_class.non_public_range(address)).to be_nil
      end
    end

    it 'refuses what it cannot parse: a hostname, garbage, nothing' do
      ['mail.example.com', 'not-an-ip', '999.1.1.1', '', nil].each do |junk|
        expect(described_class.non_public_range(junk)).to eq('unparseable-address'), "expected #{junk.inspect} to be refused"
      end
    end

    it 'refuses a scoped IPv6 address (Ruby parses the zone id, so it is judged as the link-local address it is)' do
      expect(described_class.non_public_range('fe80::1%eth0')).to eq('link-local')
    end

    it 'tolerates surrounding whitespace the way a resolver result might carry it' do
      expect(described_class.non_public_range(" 10.0.0.1\n")).to eq('private-rfc1918')
      expect(described_class.non_public_range(" 8.8.8.8\n")).to be_nil
    end
  end

  describe '.refusal' do
    it 'is nil when every address is public' do
      expect(described_class.refusal(%w[8.8.8.8 2606:4700:4700::1111])).to be_nil
    end

    it 'is nil for no addresses at all (a null-MX / no-route domain is the gems to fail, not the guards)' do
      expect(described_class.refusal([])).to be_nil
      expect(described_class.refusal(nil)).to be_nil
    end

    it 'refuses when ONLY non-public addresses were found' do
      r = described_class.refusal(%w[10.0.0.5])
      expect(r).to eq(reason: 'non_public_target', count: 1, targets: [{ address: '10.0.0.5', range: 'private-rfc1918' }])
    end

    it 'refuses a MIXED set too: the gem tries every address in order, so one public MX beside a private one still reaches it' do
      r = described_class.refusal(%w[8.8.8.8 169.254.169.254 1.1.1.1 10.0.0.5])
      expect(r[:count]).to eq(2)
      expect(r[:targets].map { |t| t[:address] }).to eq(%w[169.254.169.254 10.0.0.5]) # the public ones are not reported
    end

    it 'caps what it reports but not what it counts' do
      r = described_class.refusal((1..20).map { |i| "10.0.0.#{i}" })
      expect(r[:count]).to eq(20)
      expect(r[:targets].size).to eq(described_class::MAX_REPORTED)
    end
  end

  describe 'the override (the socket-proof harness only)' do
    around do |example|
      old = ENV[described_class::ALLOW_ENV]
      example.run
    ensure
      old.nil? ? ENV.delete(described_class::ALLOW_ENV) : ENV[described_class::ALLOW_ENV] = old
    end

    it 'is OFF by default: the guard refuses, and there is no audit warning' do
      ENV.delete(described_class::ALLOW_ENV)
      expect(described_class.refusal(%w[127.0.0.1])).not_to be_nil
      expect(described_class.override_warning).to be_nil
    end

    it 'switches the guard off ONLY for the exact value "1", and then the audit warning appears' do
      ENV[described_class::ALLOW_ENV] = '1'
      expect(described_class.refusal(%w[127.0.0.1])).to be_nil
      expect(described_class.override_warning).to include('OFF').and include('PROBE_ALLOW_NON_PUBLIC')
    end

    it 'does not treat other truthy-looking values as the override (true, yes, 0, on, empty)' do
      ['true', 'yes', '0', 'on', ''].each do |v|
        ENV[described_class::ALLOW_ENV] = v
        expect(described_class.refusal(%w[127.0.0.1])).not_to be_nil, "#{v.inspect} must not disable the guard"
        expect(described_class.override_warning).to be_nil
      end
    end
  end
end
