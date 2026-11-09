# frozen_string_literal: true

# FORK EDIT (Phase 2): request specs for the /v1 wrapper (§4). These prove routing, auth, and
# serialization SHAPE with the gem STUBBED (Truemail.validate / .host_audit / .configuration) —
# fast, no socket. The socket-level end-to-end proof (real smtp_mock/dns_mock, Appendix B matrix)
# lives outside rspec (see the P2 proof harness) so the wire path is exercised for real too.
RSpec.describe 'Wrapper /v1', type: :request do
  let(:token) { 'token' }

  before { header('AUTHORIZATION', token) }

  # A minimal fake gem Result (Truemail::Validator::Result-shaped) for stubbing. `configuration`
  # is included because the wrapper's stock_configuration re-serializes the validator via the gem's
  # own ValidatorJson, which reads result.configuration — a real Result always has it.
  def fake_result(email:, success:, smtp_debug: nil, domain: 'example.com', mail_servers: ['1.2.3.4'], errors: {})
    double(
      email: email, success: success, smtp_debug: smtp_debug,
      domain: domain, mail_servers: mail_servers, errors: errors,
      configuration: Truemail.configuration
    )
  end

  def fake_validator(result:, validation_type: :smtp)
    double(result: result, validation_type: validation_type)
  end

  describe 'auth gate' do
    it 'rejects a missing/invalid token with 401' do
      header('AUTHORIZATION', nil)
      get('/v1/stats')
      expect(response).to be_unauthorized
    end
  end

  describe 'GET /v1/stats' do
    it 'returns the counters shape' do
      get('/v1/stats')
      expect(response).to be_ok
      body = JSON.parse(response.body)
      expect(body['wrapper_api_version']).to eq('v1')
      expect(body['counters']).to include('verify_total', 'smtp_sessions', 'catchall_probes')
      expect(body['engine']).to include('core', 'version')
    end
  end

  describe 'GET /v1/config (closes GAP-3)' do
    it 'exposes the FULL effective config incl. verifier_email + timeouts + smtp_port' do
      get('/v1/config')
      expect(response).to be_ok
      cfg = JSON.parse(response.body)['effective_config']
      # The stock /verify JSON drops these — the whole point of /v1/config (§4.6).
      expect(cfg).to include('verifier_email', 'verifier_domain', 'smtp_port',
                             'connection_timeout', 'response_timeout', 'connection_attempts')
    end
  end

  describe 'POST /v1/verify' do
    # These examples stub Truemail.validate with strict arguments; the destination guard's pre-flight (a second validate with
    # `with: :mx`) is its own subject below, so it is stubbed to "public" here. The guard itself is NOT stubbed anywhere below.
    before { allow(TruemailServer::Wrapper::ProbeGuard).to receive(:preflight).and_return(nil) }

    it 'rejects a malformed body with 400' do
      post('/v1/verify', 'not json', 'CONTENT_TYPE' => 'application/json')
      expect(response.status).to eq(400)
    end

    it 'rejects a missing email with 400' do
      post('/v1/verify', {}.to_json, 'CONTENT_TYPE' => 'application/json')
      expect(response.status).to eq(400)
    end

    it 'returns the additive superset incl. the mx block (closes GAP-2) for a :regex request' do
      result = fake_result(email: 'a@b.com', success: true, domain: 'b.com', mail_servers: [])
      allow(Truemail).to receive(:validate).with('a@b.com', with: :regex)
                                           .and_return(fake_validator(result: result, validation_type: :regex))
      post('/v1/verify', { email: 'a@b.com', validation_type: 'regex' }.to_json, 'CONTENT_TYPE' => 'application/json')
      expect(response).to be_ok
      body = JSON.parse(response.body)
      expect(body['wrapper_api_version']).to eq('v1')
      expect(body['mx']).to include('domain' => 'b.com', 'mail_servers' => [])
      # :regex is not :smtp → the catch-all probe must NOT run.
      expect(body).not_to have_key('catchall')
    end

    it 'runs the catch-all probe for a :smtp request (two-session Option B, ADR-0007)' do
      target = fake_result(email: 't@corp.com', success: true, domain: 'corp.com')
      probe  = fake_result(email: 'nx@corp.com', success: false, domain: 'corp.com',
                           smtp_debug: [double(host: '1.2.3.4',
                                               response: double(port_opened: true, connection: true, helo: true,
                                                                 mailfrom: false, rcptto: false,
                                                                 errors: { rcptto: "550 User not found\n" }))])
      # Two separate Truemail.validate calls (target, then probe) — the gem is untouched.
      allow(Truemail).to receive(:validate).with('t@corp.com').and_return(fake_validator(result: target))
      allow(Truemail).to receive(:validate).with(/@corp\.com\z/, with: :smtp).and_return(fake_validator(result: probe))
      post('/v1/verify', { email: 't@corp.com' }.to_json, 'CONTENT_TYPE' => 'application/json')
      expect(response).to be_ok
      body = JSON.parse(response.body)
      expect(body['catchall']).to include('probed' => true)
      # target accepted + probe rejected → the domain distinguishes real vs fake → not_catchall.
      expect(body['catchall']['conclusion']).to eq('not_catchall')
    end
  end

  # ADR-0021 — the SMTP probe's destination guard, through the real routes with the gem STUBBED. What matters here is ORDER (the gem's
  # SMTP step must never run for a refused target), the SHAPE of the refusal fact the brain reads, and that a public target is
  # untouched. Real resolution against an independent oracle: scripts/check-probe-guard.sh.
  describe 'the probe destination guard (ADR-0021)' do
    let(:guard) { TruemailServer::Wrapper::ProbeGuard }
    let(:fixture) { JSON.parse(File.read(File.expand_path('../fixtures/probe_refused.json', __dir__))) }

    def mx_validator(servers)
      fake_validator(result: fake_result(email: 't@evil.test', success: true, domain: 'evil.test', mail_servers: servers),
                     validation_type: :mx)
    end

    def smtp_validator(servers)
      target = fake_result(email: 't@evil.test', success: true, domain: 'evil.test', mail_servers: servers)
      fake_validator(result: target)
    end

    def verify(email = 't@evil.test')
      post('/v1/verify', { email: email }.to_json, 'CONTENT_TYPE' => 'application/json')
      JSON.parse(response.body)
    end

    it 'refuses a target that resolves to non-public addresses, and the gem\'s SMTP step NEVER runs' do
      allow(Truemail).to receive(:validate).with('t@evil.test', with: :mx).and_return(mx_validator(%w[169.254.169.254 8.8.8.8 10.0.0.5]))
      expect(Truemail).not_to receive(:validate).with('t@evil.test') # the SMTP-typed call (default type), i.e. the connection
      expect(Truemail).not_to receive(:validate).with('t@evil.test', with: :smtp)
      body = verify
      expect(response).to be_ok
      expect(body['success']).to be(false)
      expect(body['smtp_debug']).to be_nil
      expect(body['probe_refused']).to include('reason' => 'non_public_target', 'count' => 2)
      expect(body).not_to have_key('catchall')
    end

    it 'emits EXACTLY the wire shape the brain\'s decoder is tested against (golden fixture, shared with the Go side)' do
      allow(Truemail).to receive(:validate).with('t@evil.test', with: :mx).and_return(mx_validator(%w[169.254.169.254 8.8.8.8 10.0.0.5]))
      body = verify
      expect(body.merge('date' => '<date>')).to eq(fixture)
    end

    it 'CONTROL: a public target proceeds to the SMTP step untouched' do
      allow(Truemail).to receive(:validate).with('t@evil.test', with: :mx).and_return(mx_validator(%w[93.184.216.34]))
      allow(Truemail).to receive(:validate).with('t@evil.test').and_return(smtp_validator(%w[93.184.216.34]))
      allow(Truemail).to receive(:validate).with(/@evil\.test\z/, with: :mx).and_return(mx_validator(%w[93.184.216.34]))
      allow(Truemail).to receive(:validate).with(/@evil\.test\z/, with: :smtp)
                                           .and_return(smtp_validator(%w[93.184.216.34]))
      body = verify
      expect(body).not_to have_key('probe_refused')
      expect(body['success']).to be(true)
      expect(Truemail).to have_received(:validate).with('t@evil.test')
    end

    it 'POST-CHECK: if the SMTP run itself used a non-public address (a DNS answer that flipped), the transcript is DISCARDED' do
      allow(Truemail).to receive(:validate).with('t@evil.test', with: :mx).and_return(mx_validator(%w[93.184.216.34]))
      leaked = double(host: '10.0.0.5', response: double(port_opened: true, connection: true, helo: true, mailfrom: false,
                                                          rcptto: false, errors: { rcptto: "550 INTERNAL BANNER SECRET\n" }))
      target = fake_result(email: 't@evil.test', success: false, domain: 'evil.test',
                           mail_servers: %w[93.184.216.34 10.0.0.5], smtp_debug: [leaked])
      allow(Truemail).to receive(:validate).with('t@evil.test').and_return(fake_validator(result: target))
      body = verify
      expect(body['probe_refused']).to include('reason' => 'non_public_target', 'count' => 1)
      expect(response.body).not_to include('INTERNAL BANNER SECRET') # never returned, therefore never persisted
      expect(body['smtp_debug']).to be_nil
    end

    it 'does not run for a request that opens no SMTP session (a :regex request has no destination)' do
      expect(guard).not_to receive(:preflight)
      result = fake_result(email: 'a@b.com', success: true, domain: 'b.com', mail_servers: [])
      allow(Truemail).to receive(:validate).with('a@b.com', with: :regex).and_return(fake_validator(result: result, validation_type: :regex))
      post('/v1/verify', { email: 'a@b.com', validation_type: 'regex' }.to_json, 'CONTENT_TYPE' => 'application/json')
      expect(response).to be_ok
    end

    it 'counts refusals in /v1/stats so the rate of attempts is visible' do
      allow(Truemail).to receive(:validate).with('t@evil.test', with: :mx).and_return(mx_validator(%w[127.0.0.1]))
      before = TruemailServer::Wrapper::Counters.instance.snapshot[:probe_refused]
      verify
      expect(TruemailServer::Wrapper::Counters.instance.snapshot[:probe_refused]).to eq(before + 1)
    end

    it 'guards the STANDALONE catch-all probe too (GET /v1/catchall/:domain)' do
      allow(Truemail).to receive(:validate).with(/@evil\.test\z/, with: :mx).and_return(mx_validator(%w[192.168.1.1]))
      expect(Truemail).not_to receive(:validate).with(/@evil\.test\z/, with: :smtp)
      get('/v1/catchall/evil.test')
      body = JSON.parse(response.body)
      expect(response).to be_ok
      expect(body['conclusion']).to eq('inconclusive')
      expect(body['is_catchall']).to be_nil
      expect(body['probe_refused']).to include('reason' => 'non_public_target')
    end

    context 'with the override (PROBE_ALLOW_NON_PUBLIC=1, the socket-proof harness only)' do
      around do |example|
        old = ENV[guard::ALLOW_ENV]
        ENV[guard::ALLOW_ENV] = '1'
        example.run
      ensure
        old.nil? ? ENV.delete(guard::ALLOW_ENV) : ENV[guard::ALLOW_ENV] = old
      end

      it 'lets a loopback target through — and turns the host audit RED, so the brain routes it nothing' do
        allow(Truemail).to receive(:validate).with('t@evil.test').and_return(smtp_validator(%w[127.0.0.1]))
        allow(Truemail).to receive(:validate).with(/@evil\.test\z/, with: :smtp).and_return(smtp_validator(%w[127.0.0.1]))
        expect(verify).not_to have_key('probe_refused')

        allow(TruemailServer::Wrapper::ResolverHygiene).to receive(:check).and_return(nil)
        allow(Truemail).to receive(:host_audit)
          .and_return(double(result: double(current_host_ip: '203.0.113.10', warnings: {})))
        get('/v1/audit')
        audit = JSON.parse(response.body)
        expect(audit['audit_status']).to eq('red')
        expect(audit['warnings'].keys).to eq(['probe_guard'])
      end
    end
  end

  describe 'GET /v1/audit' do
    let(:hygiene) { TruemailServer::Wrapper::ResolverHygiene }

    def stub_gem_audit(warnings:)
      allow(Truemail).to receive(:host_audit)
        .and_return(double(result: double(current_host_ip: '127.0.0.1', warnings: warnings)))
    end

    # The resolver check is INJECTED here. Left to read the machine, this example would pass on a developer's PC and fail on a CI
    # runner, whose container resolv.conf carries a `search` line — the real-machine run is scripts/check-resolver-hygiene.sh.
    context 'with a clean resolver' do
      before { allow(hygiene).to receive(:check).and_return(nil) }

      it 'returns audit_status green when warnings are empty' do
        stub_gem_audit(warnings: {})
        get('/v1/audit')
        expect(response).to be_ok
        expect(JSON.parse(response.body)['audit_status']).to eq('green')
        expect(JSON.parse(response.body)['warnings']).to eq({})
      end

      it 'is still red when the GEM warns — the resolver check adds nothing and takes nothing away' do
        stub_gem_audit(warnings: { ptr: 'PTR does not match' })
        get('/v1/audit')
        body = JSON.parse(response.body)
        expect(body['audit_status']).to eq('red')
        expect(body['warnings']).to eq('ptr' => 'PTR does not match')
      end
    end

    context 'with a resolver that is not clean (search/domain line, ndots > 1, dotted hostname)' do
      before { allow(hygiene).to receive(:check).and_return('node resolver is not clean: resolv.conf has a `search` line (corp.example)') }

      it 'turns an otherwise-green audit RED, with the reason under warnings.resolver — so the brain routes no SMTP here' do
        stub_gem_audit(warnings: {})
        get('/v1/audit')
        body = JSON.parse(response.body)
        expect(body['audit_status']).to eq('red')
        expect(body['warnings'].keys).to eq(['resolver'])
        expect(body['warnings']['resolver']).to include('`search` line')
      end

      it 'keeps the gem\'s own warnings alongside it' do
        stub_gem_audit(warnings: { dns: 'dns lookup failed' })
        get('/v1/audit')
        expect(JSON.parse(response.body)['warnings'].keys).to contain_exactly('dns', 'resolver')
      end

      it 'does not mutate the warnings hash the gem returned' do
        gem_warnings = { dns: 'dns lookup failed' }
        stub_gem_audit(warnings: gem_warnings)
        get('/v1/audit')
        expect(gem_warnings).to eq(dns: 'dns lookup failed')
      end
    end
  end
end
