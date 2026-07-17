# frozen_string_literal: true

# FORK EDIT (Phase 2): the wrapper's rich serializers. The stock gem serializer
# (Truemail::Log::Serializer::ValidatorBase#smtp_debug) drops helo/mailfrom/rcptto and the
# domain/mail_servers (GAP-2), which the brain's §6.2 transcript parser needs. This wrapper
# therefore serializes the FULL per-host transcript directly from the live gem result objects
# (Truemail::Validate::Smtp::Response), plus the mx block — WITHOUT touching the gem (Door 5).
# The wrapper returns engine FACTS only; it renders no status/reason/confidence (§4.1 note).
module TruemailServer
  module Wrapper
    module Serializer
      module_function

      WRAPPER_API_VERSION = 'v1'

      # smtp_debug serializes the gem's smtp_debug (array of Truemail::Validate::Smtp::Request)
      # into the FULL per-host shape the brain parses (P2 §6.2, ground-truth §6.2): each host's
      # port_opened/connection/helo booleans, the mailfrom/rcptto Net::SMTP::Response as
      # {status,string} (or false/nil), and errors{stage => raw string}. nil on a clean success
      # (the §6.8 discriminator: success && smtp_debug==nil = clean 250).
      def smtp_debug(gem_smtp_debug)
        return nil unless gem_smtp_debug

        gem_smtp_debug.map do |smtp_request|
          r = smtp_request.response
          {
            mail_host: smtp_request.host,
            port_opened: r.port_opened,
            connection: r.connection,
            helo: r.helo,
            mailfrom: net_smtp_value(r.mailfrom),
            rcptto: net_smtp_value(r.rcptto),
            errors: (r.errors || {}).transform_values { |v| safe_utf8(v) }
          }
        end
      end

      # net_smtp_value maps a Net::SMTP::Response (success) to {status, string}; passes
      # false/nil/true through unchanged (failure/not-attempted markers) — matching the shape
      # ground-truth §6.2 captured (mailfrom:{status,string} | false, rcptto:false on failure).
      def net_smtp_value(value)
        return value unless value.respond_to?(:status) && value.respond_to?(:string)

        { status: value.status, string: safe_utf8(value.string) }
      end

      # mx closes GAP-2: the stock JSON drops domain/mail_servers, but the gem's Result struct
      # carries them. The wrapper surfaces them so the brain sees the engine's over-the-wire MX
      # (engine-authoritative for the SMTP verdict, §6.6). mail_servers is [] when MX not reached.
      def mx(result)
        {
          domain: result.domain,
          mail_servers: result.mail_servers || []
        }
      end

      def safe_utf8(str)
        return str unless str.is_a?(::String)

        str.encode('UTF-8', invalid: :replace, undef: :replace)
      end
    end
  end
end
