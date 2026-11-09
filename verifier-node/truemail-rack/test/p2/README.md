# Phase-2 socket-level proof harness (`smtp_mock` + `dns_mock`)

This is the **end-to-end wire proof** for Phase 2 (spec §3.5 / Appendix B). It exercises the
**real** truemail SMTP/DNS code path against **socket-level fakes** — not stubs — so transcript
capture, the catch-all 2nd-RCPT probe, timeouts, and 5xx/4xx bodies are proven for real.

It is a **test harness, not a deployable**: the throwaway image adds the test-group gems +
`smtp_mock`/`dns_mock` + the `smtpmock` Go binary. **None of this is in the production
`verifier-node` image** (the fork's `Gemfile` is unchanged — Door 5). The **gem is never touched**;
the probe is two `Truemail.validate` calls (Option B, [ADR-0007](../../../../Docs/adr/ADR-0007-gap13-catchall-session-two-session.md)).

## Run

```bash
cd verifier-node/truemail-rack
# 1. Build the base fork image (production) if not already built:
DOCKER_BUILDKIT=0 docker build -t truemail-verifier-node:p2 .
# 2. Build the throwaway test image (adds mocks + smtpmock binary):
DOCKER_BUILDKIT=0 docker build -f test/p2/Dockerfile -t tmvn-p2-test:latest .
# 3. Run the socket proof (exits non-zero on any failure):
docker run --rm -e VERIFIER_EMAIL=verifier@example.test -e ACCESS_TOKENS=tok \
  -v "$PWD/test/p2/socket_proof.rb:/proof.rb:ro" \
  tmvn-p2-test:latest sh -c 'cd /app && ruby /proof.rb'
```

**Rows 9–11 (2026-11-09, ADR-0021) prove the probe-destination guard against the same real DNS.** A counting listener is the destination:
guard on → the loopback MX is refused and 0 connections reach it (also for the catch-all probe); the control (override on, same destination) → the
gem connects (2 connections), so zero is an observation, not an inference. The harness sets `PROBE_ALLOW_NON_PUBLIC=1` itself for rows 1–8, whose fakes
live on loopback — that override turns a real node's audit red, so it is for this harness only.

## What it proves (Appendix B matrix: 8/8 as of 2026-07-17; 11/11 with the guard rows)

| Row | Fixture | Proves |
|---|---|---|
| 4 | smtp_mock default 250-all | accept-all → catch-all detected (GAP-14 confirmed) |
| 1 | **seeded** probe pre-listed for reject | the **only** `valid` path: target 250 + probe 550 (GAP-14 — a random probe is always 250'd) |
| 2 | `not_registered_emails:[target]` | 550 no-user on RCPT (address-fault → invalid) |
| 5 | `response_delay_rcptto > response_timeout` | timeout captured → smtp_timeout/unknown |
| 8 | closed port | `port_opened=false` → connection_failed/unknown |
| 7 | `msg_rcptto_not_registered_email:'552 …'` | a 552 mailbox-full body flows through RCPT (brain maps → valid) |
| 3 | `msg_rcptto_blacklisted_email:'554 …'` | a 5xx policy-block flows through → smtp_blocked path (§6.1) |
| delta | **stock** `blacklisted_rcptto_emails` | stock blacklist really returns **421** (4xx transient), confirming the ground-truth §3.4 421-vs-554 delta — this is *why* row 3 needs a custom 554 fixture |

## Honest limits (not faked green)

- **GAP-12** — smtp_mock has **no stateful greylist** (reject-then-accept). The greylist
  **round-trip** is NOT socket-proven here. Detection + classification of a `450` → `unknown/greylisted`
  + requeue marking is unit-proven brain-side (`control-plane/internal/smtp`); true greylist
  validation is **rollout-only** (GAP-35).
- **GAP-14** — smtp_mock is accept-all-shaped, so the `valid` proof uses a **seeded** probe. This is
  a **test artifact**; faithful reject-arbitrary-unknown is a real-box (P3) property.
- Mocks prove **code-path identity**, not **behavioral identity** with real providers (spec §3.5
  honest-limit) — that is P3 (one real box) and P7 (bounce calibration).
