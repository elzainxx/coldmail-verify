# frozen_string_literal: true

require_relative 'config/loader'

# FORK EDIT (Phase 2): mount the fork's /v1 wrapper (§4) alongside the STOCK rack. URLMap keeps
# the stock app (TruemailServer::RackCascade → Application → Router: /, /version, /healthcheck)
# byte-identical — the stock request path is unchanged and unaware of the wrapper. The /v1/*
# endpoints (verify, audit, catchall, stats, config, config/reload) live entirely in the wrapper,
# never the gem (Door 5). OQ-P2-1: namespace under /v1, keep stock endpoints intact.
map '/v1' do
  run TruemailServer::Wrapper::App.new
end

run TruemailServer::RackCascade
