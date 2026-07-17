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

  describe 'GET /v1/audit' do
    it 'returns audit_status green when warnings are empty' do
      allow(Truemail).to receive(:host_audit)
        .and_return(double(result: double(current_host_ip: '127.0.0.1', warnings: {})))
      get('/v1/audit')
      expect(response).to be_ok
      expect(JSON.parse(response.body)['audit_status']).to eq('green')
    end
  end
end
