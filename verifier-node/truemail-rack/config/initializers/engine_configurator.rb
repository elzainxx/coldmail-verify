# frozen_string_literal: true

# FORK EDIT (Phase 2): factored the truemail-configure logic out of the initializer so the
# wrapper's POST /v1/config/reload (§4.5) can PULL-reload it (OQ-P2-2: the box re-reads its own
# config source; the brain never ships secrets in a request body). Defining a module has no
# side effect — `truemail.rb` requires this and calls `configure!` to apply it at boot, exactly
# as before. The gem is untouched (Door 5): this only re-invokes the gem's public `Truemail.configure`.
module TruemailServer
  module Services
    module EngineConfigurator
      module_function

      # configure! applies the effective engine config from ENV (via System::Configuration) to
      # the pinned truemail gem. It is the exact logic the stock initializer ran inline.
      # COMMAND_LINE_ATTRS[0...-2] drops the two NON-engine params (access_tokens, log_stdout).
      def configure!
        core_params = System::Configuration::COMMAND_LINE_ATTRS[0...-2]
        ::Truemail.configure do |config|
          System::Configuration.command_line_params
                               .slice(*core_params)
                               .each { |key, value| config.public_send("#{key}=", value) }
        end
      end

      # reload! is the pull-reload for POST /v1/config/reload: re-read the box's own config
      # source (ENV, via the memoized System::Configuration — memo reset first) and re-apply.
      # A raise here means the new config is unloadable; the caller (the wrapper) keeps the box
      # running on the OLD config and returns 409 — a reload must never brick the box (§4.5).
      def reload!
        System::Configuration.send(:reset_command_line_params!)
        ::Truemail.reset_configuration!
        configure!
      end
    end
  end
end
