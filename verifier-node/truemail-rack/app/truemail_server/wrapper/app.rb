# frozen_string_literal: true

require 'json'
# Explicit requires (not glob order): app.rb references these siblings at class-definition time
# (WRAPPER_API_VERSION). require_relative dedupes with the loader's absolute-path require.
require_relative 'serializer'
require_relative 'config_view'
require_relative 'counters'
require_relative 'catchall_probe'
require_relative 'resolver_hygiene'

# FORK EDIT (Phase 2): the wrapper app — the fork's own /v1/* endpoints (§4). Mounted at /v1 by
# config.ru's URLMap, so the STOCK rack (/,  /version, /healthcheck via TruemailServer::Application)
# is byte-identical and all its specs pass verbatim (OQ-P2-1: namespace under /v1, keep stock).
#
# Door 5: every endpoint here lives in the fork wrapper, NEVER the gem. Each calls the gem's PUBLIC
# API (Truemail.validate / .host_audit / .configuration) and serializes facts — it patches nothing.
# The wrapper RENDERS NO VERDICT (§4.1 note): it returns engine facts (success, errors, smtp_debug,
# catchall, mx); status/reason/confidence are brain-only. Auth: raw node-token in Authorization
# (NOT Bearer), matched by the stock AccessToken service (Appendix A.2). The brain is the only client.
module TruemailServer
  module Wrapper
    class App
      WRAPPER_API_VERSION = TruemailServer::Wrapper::Serializer::WRAPPER_API_VERSION
      JSON_HEADERS = { 'Content-Type' => 'application/json' }.freeze
      VALIDATION_TYPES = %w[regex mx mx_blacklist smtp].freeze

      def call(env)
        request = ::Rack::Request.new(env)
        route(request)
      rescue ::StandardError => error
        # A wrapper-internal error is a 500 with a machine-parseable body (the brain maps 500 →
        # unknown/connection_failed, node-unhealthy SEAM → P4). It is NEVER a fabricated verdict.
        json(500, error: "wrapper internal error: #{error.message}")
      end

      private

      def route(request)
        return json(401, error: 'invalid truemail access token') unless token_valid?(request)

        # Under config.ru's URLMap('/v1') the mount prefix is already stripped into SCRIPT_NAME,
        # so path_info is '/verify' etc. Strip a leading /v1 defensively too, so the App is
        # correct if ever called with a full path (keeps it independently unit-testable).
        path = request.path_info.sub(%r{\A/v1(?=/)}, '')
        method = request.request_method

        case [method, path]
        in ['POST', '/verify']            then verify(request)
        in ['GET', '/audit']              then audit(request)
        in ['GET', %r{\A/catchall/.+\z}]  then catchall(request, path)
        in ['GET', '/stats']              then stats
        in ['POST', '/config/reload']     then config_reload(request)
        in ['GET', '/config']             then config_show
        else json(404, error: 'resource not found')
        end
      end

      # ── POST /v1/verify (§4.1) — the workhorse. Full engine result + catch-all probe + mx. ──
      def verify(request)
        body = parse_body(request)
        return json(400, error: 'malformed request body') unless body.is_a?(::Hash)

        email = body['email']
        return json(400, error: 'email is required') unless email.is_a?(::String) && !email.empty?

        vtype = body['validation_type']
        return json(400, error: "invalid validation_type #{vtype}") if vtype && !VALIDATION_TYPES.include?(vtype)

        counters.bump(:verify_total)
        with = vtype ? vtype.to_sym : nil
        validator = with ? ::Truemail.validate(email, with: with) : ::Truemail.validate(email)
        result = validator.result
        counters.bump(:smtp_sessions) if validator.validation_type == :smtp
        tally_transcript(result.smtp_debug)

        payload = {
          wrapper_api_version: WRAPPER_API_VERSION,
          email: result.email,
          validation_type: validator.validation_type,
          success: result.success,
          errors: presence(result.errors),
          smtp_debug: TruemailServer::Wrapper::Serializer.smtp_debug(result.smtp_debug),
          mx: TruemailServer::Wrapper::Serializer.mx(result),
          configuration: stock_configuration(validator),
          date: gem_date
        }

        # The catch-all probe runs inline for the address being checked, but ONLY when SMTP was
        # actually the validation type (a :mx/:regex path never opened a session, so a probe would
        # be meaningless) and the caller didn't opt out (probe_catchall:false). GAP-13/ADR-0007:
        # OPTION B — a second separate Truemail.validate for the probe (gem untouched).
        if probe_requested?(body) && validator.validation_type == :smtp
          counters.bump(:catchall_probes)
          payload[:catchall] = TruemailServer::Wrapper::CatchallProbe.run(
            result.domain, probe_local_part: body['probe_local_part']
          )
        end

        json(200, payload)
      end

      # ── GET /v1/audit (§4.2) SEAM → P3. host_audit shape; the GATE is the brain's (green iff warnings == {}). ──
      # ADR-0019 addendum: the node's resolver hygiene is part of that audit. The gem's own warnings (ptr/dns) are passed through
      # untouched; if the resolver is not clean (search/domain line, ndots > 1, dotted hostname) ONE more warning, `resolver`, is
      # added. Nothing else changes: the brain already treats any non-empty warnings map as red and a red node as never :smtp-eligible.
      def audit(_request)
        auditor = ::Truemail.host_audit
        result = auditor.result
        warnings = (result.warnings || {}).dup
        resolver_problem = TruemailServer::Wrapper::ResolverHygiene.check
        warnings[TruemailServer::Wrapper::ResolverHygiene::WARNING_KEY] = resolver_problem if resolver_problem
        json(200,
              wrapper_api_version: WRAPPER_API_VERSION,
              current_host_ip: result.current_host_ip,
              warnings: warnings,
              audit_status: warnings.empty? ? 'green' : 'red',
              audited_at: now_rfc3339)
      end

      # ── GET /v1/catchall/:domain (§4.3) — standalone domain-level probe. ──
      def catchall(request, path)
        domain = ::Rack::Utils.unescape(path.sub(%r{\A/catchall/}, ''))
        return json(400, error: 'domain is required') if domain.empty?

        counters.bump(:catchall_probes)
        probe_local = request.params['probe_local_part']
        probe = TruemailServer::Wrapper::CatchallProbe.run(domain, probe_local_part: probe_local)
        is_catchall = case probe[:conclusion]
                      when TruemailServer::Wrapper::CatchallProbe::CONCLUSION_CATCHALL then true
                      when TruemailServer::Wrapper::CatchallProbe::CONCLUSION_NOT_CATCHALL then false
                      else nil # inconclusive → brain maps to NULL (tri-state, never overwrites, §5.6)
                      end
        json(200,
              wrapper_api_version: WRAPPER_API_VERSION,
              domain: domain,
              is_catchall: is_catchall,
              conclusion: probe[:conclusion],
              probe_local_part: probe[:probe_local_part],
              smtp_debug: probe[:smtp_debug],
              probed_at: now_rfc3339)
      end

      # ── GET /v1/stats (§4.4) SEAM → P4. Raw counter feed; no alerting. ──
      def stats
        json(200,
              wrapper_api_version: WRAPPER_API_VERSION,
              uptime_s: counters.uptime_s,
              engine: engine_versions,
              counters: counters.snapshot,
              reported_at: now_rfc3339)
      end

      # ── POST /v1/config/reload (§4.5) — PULL reload (OQ-P2-2). Never bricks the box. ──
      def config_reload(_request)
        TruemailServer::Wrapper::ConfigView # ensure loaded
        TruemailServer::Services::EngineConfigurator.reload!
        json(200,
              wrapper_api_version: WRAPPER_API_VERSION,
              reloaded: true,
              config_fingerprint: TruemailServer::Wrapper::ConfigView.advisory_fingerprint,
              reloaded_at: now_rfc3339)
      rescue ::StandardError => error
        # Unloadable config → 409, box KEEPS running the old config (§4.5). A reload must never brick.
        json(409, wrapper_api_version: WRAPPER_API_VERSION, reloaded: false, error: error.message)
      end

      # ── GET /v1/config (§4.6, closes GAP-3/OQ-11) — full effective config for the fingerprint. ──
      def config_show
        cfg = TruemailServer::Wrapper::ConfigView.effective_config
        json(200,
              wrapper_api_version: WRAPPER_API_VERSION,
              effective_config: cfg,
              config_fingerprint: TruemailServer::Wrapper::ConfigView.advisory_fingerprint(cfg),
              engine: engine_versions,
              reported_at: now_rfc3339)
      end

      # ── helpers ──────────────────────────────────────────────────────────────────────────

      def token_valid?(request)
        TruemailServer::Services::AccessToken.validate(request.get_header('HTTP_AUTHORIZATION'))
      end

      def probe_requested?(body)
        # default true for :smtp (§4.1) — only an explicit false opts out.
        body.fetch('probe_catchall', true) != false
      end

      def parse_body(request)
        raw = request.body&.read
        request.body&.rewind
        return {} if raw.nil? || raw.empty?

        ::JSON.parse(raw)
      rescue ::JSON::ParserError
        nil
      end

      # tally_transcript bumps the timeout / 5xx / 4xx counters from the transcript (§4.4 feed).
      def tally_transcript(gem_smtp_debug)
        return unless gem_smtp_debug

        gem_smtp_debug.each do |smtp_request|
          errors = smtp_request.response.errors || {}
          raw = errors[:rcptto] || errors[:mailfrom] || errors[:connection]
          next unless raw.is_a?(::String)

          if raw.match?(/timeout|timed out/i)
            counters.bump(:timeouts)
          elsif (m = raw.match(/\A\s*(\d)(\d{2})/))
            counters.bump(:"#{m[1]}xx_total") if %w[4 5].include?(m[1])
          end
        end
      end

      # stock_configuration returns the gem's stock config SUBSET (kept for parity with the stock
      # /verify JSON — NOT authoritative for the fingerprint; /v1/config §4.6 is, per the contract).
      def stock_configuration(validator)
        ::JSON.parse(::Truemail::Log::Serializer::ValidatorJson.call(validator))['configuration']
      rescue ::StandardError
        nil
      end

      def engine_versions
        { core: ::Truemail::VERSION, version: TruemailServer::VERSION }
      end

      def gem_date
        # engine local-tz string (GAP-8): the brain stores UTC and keeps this raw in the sidecar.
        ::Time.now.strftime('%Y-%m-%d %H:%M:%S %z')
      end

      def now_rfc3339
        ::Time.now.utc.strftime('%Y-%m-%dT%H:%M:%SZ')
      end

      def presence(value)
        return nil if value.nil?
        return nil if value.respond_to?(:empty?) && value.empty?

        value
      end

      def counters
        TruemailServer::Wrapper::Counters.instance
      end

      def json(status, object)
        [status, JSON_HEADERS.dup, [object.to_json]]
      end
    end
  end
end
