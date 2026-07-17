# frozen_string_literal: true

RSpec.describe RequestHelpers, type: :helper do
  include described_class

  describe '#app' do
    subject(:helper) { app }

    # FORK EDIT (Phase 2): config.ru now mounts the /v1 wrapper via URLMap alongside the stock
    # cascade (see config.ru + FORK.md), so the built app is a Rack::URLMap, not the bare
    # TruemailServer::Application. The helper's contract is "a callable Rack app built from
    # config.ru" — asserted here. Stock paths (/, /version, /healthcheck) still route to the
    # stock cascade through the URLMap, so every stock request spec passes unchanged.
    it { is_expected.to respond_to(:call) }
  end
end
