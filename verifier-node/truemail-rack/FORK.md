# Fork provenance — truemail-rack

This directory is **our fork of `truemail-rack`** — the HTTP server, and **the only thing
we fork** (Door 5: the `truemail` *engine* gem is pinned, never forked). See
[`../../PROJECT_RULES.md`](../../PROJECT_RULES.md) and [`../../Docs/adr/ADR-0003-truemail-engine-pin.md`](../../Docs/adr/ADR-0003-truemail-engine-pin.md).

## Fork base

| | |
|---|---|
| Upstream | https://github.com/truemail-rb/truemail-rack |
| Tag | `v0.9.0` |
| Commit | `9ecbbbdc45001d971a7d477e35136540c1477d11` |
| Vendored on | 2026-07-17 |
| Ruby | `3.2` (from `.ruby-version`; patch moved 3.2.0 → 3.2.11 on 2026-11-08, row 14 below) |
| Nested `.git` | removed (vendored into this monorepo, not a submodule) |

## Modifications from upstream v0.9.0 (exhaustive — keep this list complete)

Everything we change from stock is listed here. If it's not here, it's upstream verbatim.
The CI `gem-unmodified` check (see below) guarantees the *engine gem* is untouched; this
list covers *rack-side* edits (which are allowed — the rack is what we fork).

| # | File | Change | Why |
|---|---|---|---|
| 1 | `Gemfile` | `gem 'truemail', '~> 3.1'` → **`'3.3.1'`** (exact) | Door 5 exact pin (the prompt's "pin the exact version, not a range"). |
| 2 | `Gemfile` | `gem 'net-smtp', '~> 0.4.0'` → **`'0.5.1'`** (exact) | **Forced, not cosmetic:** `truemail 3.3.1` declares `net-smtp '~> 0.5.0'` on Ruby ≥ 3.1 (we pin Ruby 3.2). Upstream's `~> 0.4.0` is a **disjoint range = hard resolver conflict**. Rack-side edit only. **Phase-8:** pinned EXACTLY to `0.5.1` (the version the image resolved) — the earlier range `~> 0.5.0` let a rebuild drift within `0.5.x`; `0.5.1` satisfies `~> 0.5.0`, the engine is untouched (Door 5). |
| 3 | `Gemfile.lock` | **deleted**, regenerated in Docker | The upstream lock pinned `truemail 3.1.0`; invalid after the exact 3.3.1 pin. Host has no Ruby + rubygems HTTPS is blocked here, so the lock is resolved at `docker build` time (`bundle lock`) and becomes the build artifact. |
| 4 | `Dockerfile` (new) | added | Build + run the stock pinned rack (host has no Ruby). |

### Phase-2 modifications (the `/v1` wrapper — rack-side, **gem still untouched**)

Every Phase-2 change lives in the **fork wrapper or config**, never the gem (Door 5). The
gem-unmodified CI check still passes. The wrapper calls the gem's PUBLIC API
(`Truemail.validate` / `.host_audit` / `.configuration`) and serializes facts — it renders no
verdict (all interpretation is control-plane-side).

| # | File | Change | Why |
|---|---|---|---|
| 5 | `config.ru` | mount `map '/v1' { run TruemailServer::Wrapper::App.new }` **before** the stock `run RackCascade` (URLMap) | Adds the `/v1/*` endpoints alongside the byte-identical stock app. Stock `/`, `/version`, `/healthcheck` route through unchanged. OQ-P2-1 (namespace under `/v1`). |
| 6 | `app/truemail_server/wrapper/app.rb` (new) | the wrapper app: `POST /v1/verify`, `GET /v1/audit`, `GET /v1/catchall/:domain`, `GET /v1/stats`, `POST /v1/config/reload`, `GET /v1/config` | §4 endpoint contracts. Node-token auth (reuses stock `AccessToken`), `wrapper_api_version`, additive superset of stock JSON. |
| 7 | `app/truemail_server/wrapper/serializer.rb` (new) | rich `smtp_debug` (full per-host transcript incl. helo/mailfrom/rcptto) + `mx` block | The stock gem serializer drops these (GAP-2/§6.2 parse target). Reads live gem result objects; no gem change. |
| 8 | `app/truemail_server/wrapper/catchall_probe.rb` (new) | the catch-all probe — a **second** `Truemail.validate` for a nonexistent probe local-part (Option B) | §5 / [ADR-0007](../../Docs/adr/ADR-0007-gap13-catchall-session-two-session.md): the pinned gem exposes no reusable same-session → two sessions, gem never patched. |
| 9 | `app/truemail_server/wrapper/config_view.rb` (new) | full effective config from `Truemail.configuration` + advisory fingerprint | §4.6 (closes GAP-3/OQ-11). The brain computes the canonical fingerprint; the box's is advisory. |
| 10 | `app/truemail_server/wrapper/counters.rb` (new) | process-local `/v1/stats` counters | §4.4 raw feed (no alerting — SEAM → P4). |
| 11 | `config/initializers/engine_configurator.rb` (new) + `config/initializers/truemail.rb` | factored the inline `Truemail.configure` into `EngineConfigurator.configure!`/`reload!` | So `POST /v1/config/reload` can PULL-reload (OQ-P2-2). Boot behaviour identical; only the gem's public `Truemail.configure` is called. |
| 12 | `spec/requests/wrapper_spec.rb` (new) + `spec/support/helpers/request_helpers_spec.rb` (1 line) | wrapper request specs; relaxed the `#app` helper assertion (URLMap, not the bare `Application`) | Test the wrapper; the stock request specs pass verbatim. |
| 13 | `test/p2/` (new) | socket-level proof harness (`socket_proof.rb`, `Dockerfile`, `README.md`) | The §3.5/Appendix B end-to-end wire proof against real `smtp_mock`/`dns_mock`. A **test harness, not a deployable** — production image has none of it. |
| 14 | `.ruby-version` | `ruby-3.2.0` → **`ruby-3.2.11`** (CRLF kept) | **Forced, not cosmetic:** the Gemfile reads this file and pins Ruby EXACTLY, and the only base the Dockerfile could use was `ruby:3.2.0-slim` — a Debian 11 image whose `debian-security` repository has been removed, so a from-scratch build can no longer `apt-get install` (found 2026-11-08 by GitHub Actions; local builds had hidden it behind cached layers). No `3.2.0` image exists on Debian 12, so the base moved to the latest Ruby 3.2 patch on bookworm and this pin follows it. Same minor version; the `truemail` engine pin (Door 5) is untouched. See [ADR-0018](../../Docs/adr/ADR-0018-verifier-node-base-image.md). |
| 15 | `Gemfile.lock` (new) + `Dockerfile` (the gem-install step) | A committed lock, generated in the image's own Ruby (3.2.11) with bundler `2.7.2`; the image now `COPY`s it, runs **no** `bundle lock`, sets `bundle config set --local frozen true`, and installs bundler at the exact version in the lock's `BUNDLED WITH` | **Reproducibility, not a feature:** the Dockerfile re-resolved every gem on every build, so only `truemail 3.3.1` and `net-smtp 0.5.1` were pinned and `rack`/`thin`/`eventmachine`/`dry-*` floated. Rack-side only — the engine gem is untouched (Door 5: `assert-gem-unmodified.sh` still passes, and `scripts/check-gemfile-lock.sh` now also asserts the lock's `truemail` equals the Gemfile's exact pin). ADR-0018 addendum. |

| — | everything else | **unchanged** | Stock fork. **No gem patch** — the engine is the pinned `truemail 3.3.1`, byte-identical. |

### NOT changed (deliberately — still out of scope)

- **No gem patch.** All validation behavior is the pinned `truemail 3.3.1`, unmodified — the
  gem-unmodified CI check (`bin/assert-gem-unmodified.sh`) guarantees it. The wrapper adds behavior
  the gem doesn't offer (2nd-RCPT probe, full-config exposure, richer serialization) using the
  gem's own primitives, never by editing it.
- **No wrapper-owned SMTP client / no same-session (Option A).** Per ADR-0007 the catch-all probe
  is two `Truemail.validate` calls; the wrapper does **not** open its own `Net::SMTP` session.
- **The fork `Gemfile` is not extended** for the mock substrate — `smtp_mock`/`dns_mock` and the
  `smtpmock` binary live only in the throwaway `test/p2` image, never the production image.

## Spec-vs-reality notes surfaced by the fork (flagged, see Docs/truemail-ground-truth.md)

- The phase specs' appendices cite `rack@0.10.0` and `gem@3.3.0` illustratively. The real
  latest release tags are **rack v0.9.0** and **gem v3.3.1** — no `0.10.0` rack tag exists.
  These are *illustrative example strings* in the specs, not frozen contracts, but they are
  logged as ground-truth deltas so `engine_version` expectations track reality.
- Stock rack exposes **no `host_audit`/`/audit`** endpoint; `host_audit` is a *gem primitive*
  the Phase-2 fork must surface. Phase 0 does not add it.

## How to update the fork base (future)

Re-vendoring a newer upstream tag is a deliberate, reviewed act: re-clone at the new tag,
re-apply the (small) modification list above, regenerate the lock in Docker, update this file
and ADR-0003, and re-run the gem-unmodified check. Never let the engine gem drift off its pin.
