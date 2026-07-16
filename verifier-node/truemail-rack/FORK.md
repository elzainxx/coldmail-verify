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
| Ruby | `3.2` (from `.ruby-version`) |
| Nested `.git` | removed (vendored into this monorepo, not a submodule) |

## Modifications from upstream v0.9.0 (exhaustive — keep this list complete)

Everything we change from stock is listed here. If it's not here, it's upstream verbatim.
The CI `gem-unmodified` check (see below) guarantees the *engine gem* is untouched; this
list covers *rack-side* edits (which are allowed — the rack is what we fork).

| # | File | Change | Why |
|---|---|---|---|
| 1 | `Gemfile` | `gem 'truemail', '~> 3.1'` → **`'3.3.1'`** (exact) | Door 5 exact pin (the prompt's "pin the exact version, not a range"). |
| 2 | `Gemfile` | `gem 'net-smtp', '~> 0.4.0'` → **`'~> 0.5.0'`** | **Forced, not cosmetic:** `truemail 3.3.1` declares `net-smtp '~> 0.5.0'` on Ruby ≥ 3.1 (we pin Ruby 3.2). Upstream's `~> 0.4.0` is a **disjoint range = hard resolver conflict**. Rack-side edit only. |
| 3 | `Gemfile.lock` | **deleted**, regenerated in Docker | The upstream lock pinned `truemail 3.1.0`; invalid after the exact 3.3.1 pin. Host has no Ruby + rubygems HTTPS is blocked here, so the lock is resolved at `docker build` time (`bundle lock`) and becomes the build artifact. |
| 4 | `Dockerfile` (new) | added | Build + run the stock pinned rack (host has no Ruby). |
| — | everything else | **unchanged** | Stock fork. No wrapper endpoints, no gem patch. |

### NOT changed (deliberately — these are Phase 2, not Phase 0)

- **No wrapper endpoints.** Stock rack serves only `/` (validation), `/version`, `/healthcheck`
  (confirmed in `app/truemail_server/router.rb`). The Phase-2 `/verify`, `/audit`,
  `/catchall/:domain`, `/config`, `/config/reload`, `/stats` endpoints are **not** added here.
- **No gem patch.** All validation behavior is the pinned `truemail 3.3.1`, unmodified.

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
