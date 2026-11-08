# ADR-0018 — The verifier-node base image is pinned by digest on a supported Debian, and CI builds it without a cache

- **Status:** ACCEPTED (2026-11-08) — found the first time GitHub Actions built the image from scratch.
- **Kind:** build/supply-chain hygiene. **Door 5 is untouched**: it protects the `truemail` *gem* (pinned `3.3.1`,
  asserted unmodified), not the OS image or the interpreter patch level. No contract, vocabulary, schema or response
  shape changes.
- **Deciders:** owner (GitHub-Actions-green brief, chunk C2 item 2).
- **Governs:** `verifier-node/truemail-rack/Dockerfile` · `verifier-node/truemail-rack/.ruby-version` ·
  `.github/workflows/ci.yml` (`base-image-pinned` job and the two `docker build` steps) ·
  `scripts/check-base-image-pinned.sh` and its paired `scripts/check-base-image-pinned-selftest.sh` ·
  `verifier-node/truemail-rack/FORK.md` (row 14 and the Ruby row).

---

## Context

The verifier-node image was `FROM ruby:3.2.0-slim`. That tag is **Debian 11 (bullseye)**. Debian removed bullseye's
`debian-security` repository from the main mirrors, so `apt-get install build-essential ca-certificates` — the second
step of the Dockerfile — returns `404` on any build that does not already hold that layer.

Nothing noticed, because nothing ever built the image from scratch: every local build reused a cached layer, so the
image looked healthy for months while being **impossible to build**. It surfaced on 2026-11-08, the first time a real
GitHub runner (no cache) ran the `door5-gem-unmodified` and `verifier-node-rspec` jobs; both failed at that `apt-get`.
It is the same shape as the `verification_raw` partition cliff (ADR-0017): a failure whose trigger is a **date and the
absence of a warm cache**, invisible for exactly as long as both held.

No `ruby:3.2.0-*` image exists on bookworm. The newest 3.2 patch there is **3.2.11**.

## Decision

1. **Base = `ruby:3.2.11-slim-bookworm`, pinned by digest** (`@sha256:84140472…`). Same Ruby *minor* (3.2) as before —
   the brief was explicit that the minor does not move here. A tag is a moving target; a digest is a content address, so
   "the same image" is no longer a claim about a tag.
2. **`.ruby-version` moves with it** (`ruby-3.2.0` → `ruby-3.2.11`). The Gemfile reads that file and pins Ruby to it
   *exactly*, so the Dockerfile and the file drifting apart is a build failure waiting for the next from-scratch build.
   It is recorded in `FORK.md` (row 14) as a fork edit. The file is stored with CRLF in the fork; the Gemfile `.strip`s
   it, and the gate reads it with the CR removed.
3. **CI builds the image with `--pull --no-cache`**, in both jobs that build it. This is the actual control: a runner
   starts cold anyway, and the flags keep it that way if someone later "optimises" the workflow.
4. **A static gate, `scripts/check-base-image-pinned.sh`, enforces all of it** in its own job (`base-image-pinned`) so a
   red there reads as "the pin regressed" and not as a build failure inside a 380-test run:
   FROM carries `@sha256:<64 hex>`; FROM names no end-of-life Debian codename and *does* name a supported one
   (bookworm/trixie); FROM's Ruby tag equals the patch in `.ruby-version`; every `docker build` in `ci.yml` has both
   flags; and there is at least one such build (otherwise assertion 4 would be vacuous).
5. **The gate has a paired control**, `check-base-image-pinned-selftest.sh`: 11 cases that each trip one assertion and
   check both the exit code **and the message** (a gate that fails for the wrong reason is not a control). It includes
   the original defect (`ruby:3.2.0-slim-bullseye`) and the prefix trap (`3.2.1` must not satisfy `3.2.11`).

## Alternatives considered

- **Keep bullseye and point apt at `archive.debian.org`.** Makes the build pass while keeping an unmaintained OS under a
  service that accepts network input. Rejected: it hides the defect instead of removing it.
- **A floating tag (`ruby:3.2-slim`).** Would have fixed today's build and reintroduced the same class tomorrow: the
  image changes under you with no commit to point at. Rejected in favour of the digest.
- **Do nothing about the cache, only fix the base.** Fixes this instance, not the class. The class is *"a warm cache can
  hide a dead base"*, and only `--pull --no-cache` plus a check that it stays on addresses that.

## Consequences

- The image builds from scratch again. Door 5 (`assert-gem-unmodified.sh`) passes unchanged and `bundle list` still
  resolves `truemail (3.3.1)`; the fork's RSpec suite is the evidence that the engine behaves the same on 3.2.11.
- **Moving the base is now a deliberate act with a checklist:** pull the new image, take its digest, change the FROM line
  and `.ruby-version` together, run CI. The gate refuses a half-done move.
- **What this does NOT make reproducible, stated so it is not mistaken for coverage:** there is **no committed
  `Gemfile.lock`**. The Dockerfile runs `bundle lock` on every build, so only `truemail` (`3.3.1`) and `net-smtp` are
  pinned; every other gem floats to whatever resolves that day. A digest-pinned base makes the OS layer reproducible;
  it does not make the image reproducible. That is the same hidden-variable shape one layer up, and it is registered
  here as an open item rather than fixed under this ADR, because committing a lockfile is a change to the fork's
  dependency posture that the brief did not ask for.
- `check-test-enumeration.sh` requires every `check-*.sh` to be wired in `ci.yml` and named in
  `Docs/test-strategy.md` §5 (item 10); both are done.

## Report requested by the brief — Ruby 3.2 end-of-life, and what moving off it would take

*Report only. Nothing below changes the Ruby minor.*

**Is Ruby 3.2 past end-of-life? Yes.** ruby-lang.org's maintenance-branches page, read 2026-11-08, lists **3.2 as
end-of-life since 2026-04-01** (about seven months ago). The branches still supported on that date: **4.0** and **3.4**
(normal maintenance) and **3.3** (security maintenance only, until **2027-03-31**). `3.2.11` is therefore the last 3.2
release that will exist: the interpreter inside the pinned image will not receive further security fixes. Debian
bookworm continues to patch the *OS* packages, but the Ruby binary in the official image is built from upstream, so the
exposure is the interpreter itself, on a service that takes network input (`/v1`, token-authenticated). The digest pin
in this ADR makes that state *stable and visible*; it does not make it safe.

**What moving the engine's runtime to Ruby 3.3 or 3.4 would take**, from reading the repo (this part is analysis, **not
measured** — see the last paragraph):

1. *Mechanical, small:* change `.ruby-version` and the Dockerfile FROM (new digest) together, update `FORK.md` and the
   Gemfile comment that says "we pin Ruby 3.2". The gate in this ADR already enforces that those move as one.
2. *Door 5 is not the obstacle.* The gem stays `truemail 3.3.1`, byte-identical; `assert-gem-unmodified.sh` is
   independent of the interpreter. What changes is the *interpreter the pinned engine runs on*, so the evidence that
   scoring is unchanged has to be re-earned: the fork's 71 RSpec examples, and the socket-level wire proof
   (`test/p2`, which CI does not run per `ci.yml`'s own comment).
3. *Where it could break:* the Gemfile's own comment records that `truemail 3.3.1` needs `net-smtp ~> 0.5.0` on
   Ruby ≥ 3.1; Ruby 3.4 moves several more default gems to bundled gems (an unlisted `require` then fails at load); and
   `thin`/`eventmachine` are native extensions that must compile against the new interpreter. None of these is known to
   fail; none is known to pass. Because there is no lockfile (above), the transitive gem set would also move.
4. *Moving the engine itself* (`truemail` 3.3.1 → 4.0.0, the current release on rubygems, which declares
   `required_ruby_version >= 2.5.0`) is a different and larger decision: it changes Door 5's pin, ADR-0003's agreement
   and the scoring evidence, and needs its own ADR and owner ruling. It is not needed to leave Ruby 3.2.

**Not measured:** no build of this image on Ruby 3.3 or 3.4 has been run. The experiment is cheap and contained — copy
the build context out of the repo, change only the FROM tag and `.ruby-version`, build with `--pull --no-cache`, run the
same RSpec command CI runs, record what fails — and is staged as a throwaway script. It was not run in this pass because
the host ran out of memory partway through the verification sequence. Until it is run, "what would break" in point 3 is a
list of places to look, not a finding.

**Recommendation (not an action):** schedule the move to Ruby 3.4 — or 3.3 if the longer-support runway matters less
than the smaller jump — as its own change, behind the same CI, and commit a `Gemfile.lock` in the same change so the
image is reproducible. Do not read the digest pin as having closed the end-of-life exposure.

## Verification (what makes the guard a control, not a comment)

- `check-base-image-pinned.sh` prints `ok` on the tree; the selftest passes 11/11, each rejection matched on its
  message; the original defect (`3.2.0-slim-bullseye`) is one of the 11.
- Wiring is mechanical: `check-test-enumeration.sh` fails if the gate is not in `ci.yml` or not in the strategy doc.
- The image itself was rebuilt from scratch with `--pull --no-cache` on a Linux rehearsal of the runner
  (`tools/ci-replica.sh`, `door5-gem-unmodified` job): `apt-get` fetched from `bookworm-security`, Door 5's assertion
  passed, `bundle list` resolved `truemail (3.3.1)`. **Green means green on the runner:** the GitHub Actions run for
  this change is recorded in `coldmail-platform/verification-runs.md`, and that, not the rehearsal, is the result.
