# frozen_string_literal: true

require 'securerandom'

# FORK EDIT (Phase 2): the catch-all probe (§5). GAP-13 spike ruling ([ADR-0007]): the pinned
# gem exposes NO reusable same-session (Request#session_data is private + one hardcoded RCPT;
# Net::SMTP auto-closes on the block). So we use OPTION B — a SECOND, separate Truemail.validate
# for a known-nonexistent probe address at the same domain — the gem is NEVER patched (Door 5).
#
# The probe asks the domain about an address we KNOW cannot exist:
#   probe accepted (250)          -> the domain 250s a fake      -> conclusion: catchall
#   probe rejected (definitive 5xx) -> distinguishes real vs fake -> conclusion: not_catchall
#   probe transient / unreachable -> can't tell                  -> conclusion: inconclusive
#
# This is a DOMAIN-LEVEL fact (engine fact, not a verdict). The brain combines it with the
# TARGET's RCPT via the §5.5 truth table: valid is minted ONLY on target-250 + probe-rejected
# (row 4). Every other accepted path is risky. The probe address is an INSTRUMENT: never logged,
# cached, or hashed as a subject (§5.3) — that discipline is the brain's (it just isn't persisted).
module TruemailServer
  module Wrapper
    module CatchallProbe
      module_function

      CONCLUSION_CATCHALL = 'catchall'
      CONCLUSION_NOT_CATCHALL = 'not_catchall'
      CONCLUSION_INCONCLUSIVE = 'inconclusive'

      # run probes `domain` with a nonexistent local-part and returns the §4.1 `catchall` block.
      # probe_local_part is optional: prod uses a CSPRNG (OQ-P2-3); tests pass a fixed value so the
      # mock can be seeded to reject exactly it (GAP-14 — a random probe is always 250'd by the
      # accept-all mock, so the valid path is only reachable with a seeded, pre-rejected probe).
      def run(domain, probe_local_part: nil)
        local = probe_local_part && !probe_local_part.empty? ? probe_local_part : random_local_part
        probe_email = "#{local}@#{domain}"

        result = ::Truemail.validate(probe_email, with: :smtp).result
        conclusion = conclude(result)

        {
          probed: true,
          probe_local_part: local,
          accepted: result.success == true,
          conclusion: conclusion,
          smtp_debug: TruemailServer::Wrapper::Serializer.smtp_debug(result.smtp_debug)
        }
      rescue ::StandardError => error
        # A probe that blows up is INCONCLUSIVE, never a hard failure — inability to probe is
        # unknown-shaped, not an error (§4.3 failure modes). The target verdict is unaffected.
        {
          probed: true,
          probe_local_part: (defined?(local) && local) || nil,
          accepted: false,
          conclusion: CONCLUSION_INCONCLUSIVE,
          error: error.message
        }
      end

      # conclude classifies the probe result into the tri-state DOMAIN conclusion. success => the
      # domain accepted a known-fake => catchall. Otherwise inspect the transcript: a definitive
      # 5xx recipient rejection => not_catchall; a 4xx/timeout/unreachable/no-SMTP => inconclusive
      # (conservative — an inconclusive probe is treated as POSSIBLE catch-all, never not, so the
      # brain hardens toward risky, §5.5 row 5). The brain re-parses the transcript authoritatively
      # (GAP-4); this coarse conclusion is the box's summary that §4.1/§4.3 require it to return.
      def conclude(result)
        return CONCLUSION_CATCHALL if result.success == true

        code = first_rcptto_code(result.smtp_debug)
        case code && code[0]
        when '5' then CONCLUSION_NOT_CATCHALL # definitive recipient rejection: distinguishes fake
        else CONCLUSION_INCONCLUSIVE          # 4xx / timeout / never reached SMTP => can't tell
        end
      end

      # first_rcptto_code walks the transcript for the first rcptto-stage raw error string and
      # extracts its leading 3-digit code. Returns nil if SMTP was never reached (mx/connection
      # failure) — which correctly falls through to inconclusive.
      def first_rcptto_code(gem_smtp_debug)
        return nil unless gem_smtp_debug

        gem_smtp_debug.each do |smtp_request|
          errors = smtp_request.response.errors || {}
          raw = errors[:rcptto]
          next unless raw

          m = raw.match(/\A\s*(\d{3})/)
          return m[1] if m
        end
        nil
      end

      # random_local_part: 24 hex chars + an unmistakable nx- prefix so it can't be mistaken for a
      # role/alias, all-lowercase to survive the brain's norm/v1 (§5.3). CSPRNG in prod.
      def random_local_part
        "nx-#{::SecureRandom.hex(12)}"
      end
    end
  end
end
