# Truemail Verification System

An email-verification control plane built around the pinned [`truemail`](https://github.com/truemail-rb/truemail) engine.
This repository is the **project spine** produced by **Phase 0** — scaffolding, contracts,
and living documentation. It contains **no feature logic yet**; every runtime module is a
stub that *respects* a frozen contract (see [`PROJECT_RULES.md`](PROJECT_RULES.md)).

> **New here? Read [`PROJECT_RULES.md`](PROJECT_RULES.md) first.** It is the constitution:
> the five one-way doors, the two vocabularies, first-class `unknown`, shadow-mode defaults,
> and the kill-switch ladder — enough to understand the system without reading all seven specs.

---

## The two deployables (never merged)

The single most important structural fact about this repo: it holds **two decoupled
deployables**, and the boundary between them is a **security and correctness boundary**,
not a packaging convenience.

```
Codmailling/
├── control-plane/   # Go control plane (the design docs' "brain"). Holds ALL state. Port 25 CLOSED.
│                 #   front door, policy engine, cheap layers, router (node-LIST),
│                 #   scoring, the LOG, cache, secrets custody (incl. the pepper).
│
├── verifier-node/   # verifier node(s) (the design docs' "box"). Dumb, stateless, DISPOSABLE.
│   └── truemail-rack/   #   a FORK of truemail-rack embedding the PINNED truemail gem.
│                        #   Opens outbound port 25. Holds NO state, NO DB creds, NO pepper.
│                        #   Killed + re-spun freely. Runs STOCK in Phase 0.
│
├── Docs/         # Living documentation: 7 phase specs, build plan, revision-pass docs,
│   ├── adr/      #   ADRs (one-way-door + owner-OQ rulings), DECISIONS.md, the
│   └── ...       #   generated config reference, ground-truth, test strategy.
│
├── test/         # Real-shaped behavioral/integration harness (tied to named GAPs).
├── .github/      # CI: build + tests + gem-unmodified + docs-updated + code-review gates.
├── PROJECT_RULES.md   # The constitution (read first).
├── NOTICE             # Third-party attributions (truemail + rack are MIT — respected).
└── README.md
```

### Why the boundary is load-bearing

| Property | The control-plane | The verifier-node |
|---|---|---|
| **State** | holds **all** of it (Supabase) | holds **none** — pure function `email → raw engine JSON` |
| **Secrets** | pepper, node tokens, vendor keys, Supabase key | only its own node token + verifier identity |
| **Port 25** | **closed** (never sends) | **open outbound** (the whole reason a verifier-node exists) |
| **Disposability** | long-lived, protected | cattle: kill + re-spin loses nothing |
| **Language** | Go | Ruby (forked rack + pinned gem) |
| **Blast radius if compromised** | the data moat | one verifier-node's config — no history, no pepper |

Because the verifier-node holds no state, a compromised or IP-burned verifier-node is **disposable** — this is a
security dividend of the design, not just an architecture note. The control-plane never ships the
pepper or DB credentials to a verifier-node.

## Door 5 — the engine is pinned, never forked

- **`truemail` (the engine)** is a **pinned dependency at `3.3.1`** ([ADR-0003](Docs/adr/ADR-0003-truemail-engine-pin.md)) — **never patched, never forked.**
- **`truemail-rack` (the HTTP server)** is the **only** thing we fork (`verifier-node/truemail-rack/`).
- CI asserts the vendored gem is byte-for-byte upstream (`.github/` + `verifier-node/` check).
- Any behavioral gap is closed **control-plane-side** or in the **rack wrapper** (Phase 2+), never by editing the gem.

In Phase 0 the fork runs **stock** — no `/verify`, `/audit`, `/catchall`, `/config`, or
`/stats` wrapper endpoints (those are Phase 2). See [`verifier-node/truemail-rack/FORK.md`](verifier-node/truemail-rack/FORK.md).

## Build & run (Phase 0)

Phase 0 proves both deployables **build empty**. Neither does real verification yet.

```bash
# Control-plane (Go 1.26+): builds clean, serves a stubbed /v1/verify + healthz.
cd control-plane && go build ./... && go test ./...

# Verifier-node (Docker; Ruby not required on the host): builds the pinned stock rack.
docker compose -f verifier-node/docker-compose.yml build
docker compose -f verifier-node/docker-compose.yml up   # stock rack on :3000 (root=/, /version, /healthcheck)
```

> **Toolchain note (this environment):** Go, git, and Docker are present; Ruby is **not**
> installed on the host, so the verifier-node builds and runs **only via Docker** — which matches the
> build plan's "get the stock container running locally." See
> [`Docs/truemail-ground-truth.md`](Docs/truemail-ground-truth.md) for what a live run captures
> and reconciles against the specs.

## License posture

- **Third-party components are MIT and are respected/attributed** in [`NOTICE`](NOTICE)
  (the `truemail` engine and the `truemail-rack` fork, both © Vladislav Trotsenko).
- **This project's own license is an OWNER decision and is deliberately not asserted yet.**
  Pick one and add a top-level `LICENSE` before any external distribution. Flagged, not chosen
  — consistent with the "flag, don't decide" discipline of this build.

## Where to go next

| You want to… | Read |
|---|---|
| Understand the rules that can never change | [`PROJECT_RULES.md`](PROJECT_RULES.md) |
| See every toggle/flag/secret/dial | [`Docs/config-reference.md`](Docs/config-reference.md) (generated) |
| See the binding cross-phase rulings | [`Docs/DECISIONS.md`](Docs/DECISIONS.md) |
| See one-way-door decisions | [`Docs/adr/`](Docs/adr/) |
| Understand what real truemail emits vs. what the specs assumed | [`Docs/truemail-ground-truth.md`](Docs/truemail-ground-truth.md) |
| Understand the test discipline | [`Docs/test-strategy.md`](Docs/test-strategy.md) |
| Know what must be true before Phase 1 code | [`Docs/pre-takeoff-checklist.md`](Docs/pre-takeoff-checklist.md) |
