# frozen_string_literal: true

require 'tempfile'

# FORK EDIT (ADR-0019 addendum): the node's resolver hygiene check. Every input is INJECTED (a resolv.conf written to a tempfile, a
# hostname string) — these specs never read the machine they run on, which matters: a GitHub runner's container has a `search` line,
# so a spec that read the real file would be red there and green on a developer's PC. The run against the REAL Ruby resolver, real
# mounted resolv.conf and real hostname is scripts/check-resolver-hygiene.sh. Each "flagged" example has a clean twin next to it.
RSpec.describe TruemailServer::Wrapper::ResolverHygiene do
  def with_resolv_conf(text)
    Tempfile.create('resolv.conf') do |f|
      f.write(text)
      f.flush
      yield f.path
    end
  end

  def check(text, hostname: 'box')
    with_resolv_conf(text) { |path| described_class.check(resolv_conf: path, hostname: hostname) }
  end

  describe 'a clean resolver' do
    it 'is nil: only nameservers, a short hostname' do
      expect(check("nameserver 10.0.0.2\nnameserver 10.0.0.3\n")).to be_nil
    end

    it 'is nil when resolv.conf does not exist at all (a determinate fact: nothing to flag)' do
      expect(described_class.check(resolv_conf: '/nonexistent/resolv.conf', hostname: 'box')).to be_nil
    end

    it 'ignores commented-out search/domain/options lines' do
      expect(check("# search corp.example\n; domain corp.example\n  # options ndots:5\nnameserver 10.0.0.2\n")).to be_nil
    end

    it 'allows ndots 0 and 1 (Resolv\'s own default is 1)' do
      expect(check("nameserver 10.0.0.2\noptions ndots:1\n")).to be_nil
      expect(check("nameserver 10.0.0.2\noptions ndots:0\n")).to be_nil
    end

    it 'allows unrelated options' do
      expect(check("nameserver 10.0.0.2\noptions timeout:2 attempts:3 rotate\n")).to be_nil
    end
  end

  describe 'a `search` line' do
    it 'turns it red' do
      msg = check("nameserver 10.0.0.2\nsearch corp.example internal.example\n")
      expect(msg).to include('`search` line').and include('corp.example internal.example')
    end
  end

  describe 'a `domain` line' do
    it 'turns it red' do
      expect(check("nameserver 10.0.0.2\ndomain corp.example\n")).to include('`domain` line')
    end
  end

  describe 'ndots greater than 1' do
    it 'turns it red at 2 and above, however it is spelled among other options' do
      expect(check("nameserver 10.0.0.2\noptions ndots:2\n")).to include('ndots:2')
      expect(check("nameserver 10.0.0.2\noptions timeout:1 ndots:3 rotate\n")).to include('ndots:3')
      expect(check("nameserver 10.0.0.2\noptions ndots:15\n")).to include('ndots:15')
    end

    it 'is read from ANY options line' do
      expect(check("options timeout:1\nnameserver 10.0.0.2\noptions ndots:2\n")).to include('ndots:2')
    end
  end

  describe 'a hostname with a dot (Ruby derives a search domain from it, even with a clean resolv.conf)' do
    it 'turns it red' do
      expect(check("nameserver 10.0.0.2\n", hostname: 'verifier.example.com')).to include('hostname "verifier.example.com"')
    end

    it 'does not when the hostname is short (the clean twin)' do
      expect(check("nameserver 10.0.0.2\n", hostname: 'verifier')).to be_nil
    end
  end

  describe 'a resolv.conf that exists but cannot be read' do
    it 'is red: the gate does not guess at what it could not see' do
      Dir.mktmpdir do |dir| # reading a directory fails with EISDIR, a SystemCallError other than ENOENT
        expect(described_class.check(resolv_conf: dir, hostname: 'box')).to include('cannot read').and include('cannot be shown to be clean')
      end
    end
  end

  describe 'several problems at once' do
    it 'are reported together, in ONE string (it becomes the single warnings[:resolver] value)' do
      msg = check("search corp.example\ndomain corp.example\noptions ndots:2\n", hostname: 'a.b')
      expect(msg).to be_a(String)
      expect(msg).to include('`search` line').and include('`domain` line').and include('ndots:2').and include('hostname "a.b"')
    end

    it 'names the remedy, so an operator who sees the red knows what to change' do
      expect(check("search corp.example\n")).to include('no search domain').and include('hostname with no dot')
    end
  end

  describe '.analyze (pure)' do
    it 'maps text to problems and nothing else' do
      expect(described_class.analyze("nameserver 1.1.1.1\n")).to eq([])
      expect(described_class.analyze("search a\n").size).to eq(1)
      expect(described_class.analyze(nil)).to eq([])
    end
  end

  it 'is keyed :resolver, the key the brain receives in the audit warnings map' do
    expect(described_class::WARNING_KEY).to eq(:resolver)
  end
end
