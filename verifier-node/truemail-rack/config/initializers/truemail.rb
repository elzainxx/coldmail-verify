# frozen_string_literal: true

require 'truemail'
require_relative 'engine_configurator'

# FORK EDIT (Phase 2): the inline configure block moved into EngineConfigurator (so
# POST /v1/config/reload can re-run it — §4.5). Behaviour at boot is identical: apply the
# effective engine config from ENV to the pinned gem. The gem's public API is all that's called.
TruemailServer::Services::EngineConfigurator.configure!
