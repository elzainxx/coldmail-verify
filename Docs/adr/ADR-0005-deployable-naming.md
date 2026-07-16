# ADR-0005 — The two deployables are named `control-plane` and `verifier-node`

**Status:** ACCEPTED — 2026-07-17 (Phase 0 closeout)
**Door:** none — a naming decision (durable, but touches no frozen contract)
**Deciders:** owner (Phase-0 closeout instruction)
**Companions:** [`../../README.md`](../../README.md) · [`../../PROJECT_RULES.md`](../../PROJECT_RULES.md) · [ADR-0004](ADR-0004-cheap-layers-brain-native.md)

---

## Context

Through the seven design specs the two deployables were referred to by their whiteboard names:
the **"brain"** (the Go control plane) and the **"box"** (the disposable verifier node). Those
names were fine for design discussion but are informal for a real codebase — a directory named
`brain/` and a binary named `brain` read as jargon, and "box" is ambiguous (container? server?).
Phase-0 closeout promotes them to real names.

Naming is recorded as an ADR because it is a **durable decision**: it fixes directory names, the
Go module path, the binary name, config keys, CI job names, and the vocabulary of every doc — all
things later work and external references depend on.

## Decision

| Whiteboard name (design docs) | Real name (code + this repo) |
|---|---|
| **brain** | **`control-plane`** — the Go control plane; holds all state; port 25 closed |
| **box** | **`verifier-node`** — the disposable verifier node (forked truemail-rack + pinned gem) |

Concretely renamed (total, this pass):
- Directories: `brain/` → `control-plane/`, `box/` → `verifier-node/`, `control-plane/cmd/brain/` → `control-plane/cmd/control-plane/`.
- Go module path: `…/brain` → `…/control-plane`; all imports updated.
- Config/CI/scripts: kill-switch env vars unchanged (`TM_*`); CI job names, working-directory, Docker image/service names, and the docs-updated path globs updated to the new paths.
- Docs: README, PROJECT_RULES, NOTICE, .gitignore, the Docs/ Phase-0 artifacts, and the ADR prose all use the new names.

## What is deliberately NOT renamed

- **The seven design specs, the build plan, and the five revision-pass docs** keep "brain"/"box" —
  they are the **historical design vocabulary**, quoted as-is. The prompt's rule: no lingering
  "brain"/"box" in code or docs **except where quoting the original design docs**. A one-line gloss
  in README maps the old names to the new so a reader connecting old discussions to new code isn't lost.
- **ADR-0004's filename** (`ADR-0004-cheap-layers-brain-native.md`) keeps "brain-native" — ADRs are
  immutable records identified by number; renaming the file would break durable references. Its
  *content* uses the new names, with a terminology note.

## Consequences

- **Positive:** the codebase reads in real terms; the two-deployable boundary language is consistent
  end to end; external references (module path, binary, CI) are stable going forward.
- **Neutral (by construction):** **no frozen door, contract, schema, vocabulary, or dial changed** —
  this is a pure rename. The `truemail` pin, the five doors, the Door-4 enums, and every config key are
  byte-identical; only names of *our* directories/module/docs moved. The full brain test suite passes
  unchanged under the new module path.
- **Cost:** the design specs now use different names than the code — mitigated by this ADR + the README
  gloss being the single mapping of record.
