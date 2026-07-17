# frozen_string_literal: true

require 'digest'

# FORK EDIT (Phase 2): exposes the gem's FULL effective config (§4.6, closes GAP-3/OQ-11).
# The stock validation JSON embeds only a config SUBSET (no verifier_email/domain, timeouts,
# smtp_port, connection_attempts) — so the brain's config_fingerprint can't be computed from a
# /verify response alone. This reads the pinned gem's live Truemail.configuration and reports the
# full verdict-affecting set (P1 §3.2.5 input list). The BRAIN computes the canonical fingerprint
# (§4.6 fingerprint authority); the box's own hash here is ADVISORY/cross-check only.
module TruemailServer
  module Wrapper
    module ConfigView
      module_function

      # effective_config reads the live pinned-gem configuration into the §4.6 shape. Regexes are
      # reported as source strings (JSON-serializable); the pattern being the gem default is
      # surfaced as the sentinel "default gem value" (mirrors the gem's own serializer) so a
      # default vs a custom pattern fingerprint distinctly.
      def effective_config
        c = ::Truemail.configuration
        {
          verifier_email: c.verifier_email,
          verifier_domain: c.verifier_domain,
          default_validation_type: c.default_validation_type,
          validation_type_for: c.validation_type_by_domain,
          smtp_safe_check: c.smtp_safe_check,
          smtp_fail_fast: c.smtp_fail_fast,
          not_rfc_mx_lookup_flow: c.not_rfc_mx_lookup_flow,
          email_pattern: pattern(c.email_pattern, ::Truemail::RegexConstant::REGEX_EMAIL_PATTERN),
          smtp_error_body_pattern: pattern(c.smtp_error_body_pattern, ::Truemail::RegexConstant::REGEX_SMTP_ERROR_BODY_PATTERN),
          connection_timeout: c.connection_timeout,
          response_timeout: c.response_timeout,
          connection_attempts: c.connection_attempts,
          smtp_port: c.smtp_port,
          blacklisted_mx_ip_addresses: c.blacklisted_mx_ip_addresses,
          whitelisted_domains: c.whitelisted_domains,
          blacklisted_domains: c.blacklisted_domains,
          whitelist_validation: c.whitelist_validation,
          dns: c.dns
        }
      end

      # advisory_fingerprint is the box's own SHA-256 over a sorted-key serialization of the
      # effective config. It is a cross-check ONLY — the brain recomputes the canonical hash
      # (§4.6). Prefixed cfg_sha256: to match the brain's format.
      def advisory_fingerprint(cfg = effective_config)
        canonical = cfg.sort_by { |k, _| k.to_s }
                       .map { |k, v| "#{k}=#{canonical_value(v)}" }
                       .join("\n")
        "cfg_sha256:#{::Digest::SHA256.hexdigest(canonical)[0, 16]}"
      end

      def pattern(actual, default)
        return TruemailServer::Wrapper::Serializer::DEFAULT_PATTERN if defined?(TruemailServer::Wrapper::Serializer::DEFAULT_PATTERN) && actual == default
        return 'default gem value' if actual == default

        actual.respond_to?(:source) ? actual.source : actual.to_s
      end

      def canonical_value(value)
        case value
        when ::Hash  then value.sort_by { |k, _| k.to_s }.map { |k, v| "#{k}:#{v}" }.join(',')
        when ::Array then value.map(&:to_s).sort.join(',')
        else value.to_s
        end
      end
    end
  end
end
