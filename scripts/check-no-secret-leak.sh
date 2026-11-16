#!/usr/bin/env bash
# no-secret-leak — BLOCKING CI gate. Boots the REAL control-plane binary with every secret-bearing
# env var set to a distinct canary, captures its process log, and FAILS if any canary appears.
#
# WHY THIS EXISTS (fix-plan 14.4 / D12). M1 had no automated log-scanning gate, and that absence is
# what let U2/D12 sit undetected: `main.go` logs `store.OpenPG`'s error verbatim at boot, and the
# pinned pgx v5.10.0 embeds the whole connection string — password included — in its parse error. The
# code fix (store.scrubDSNError) is unit-tested in internal/store/dsn_redaction_test.go, but a unit
# test only guards the ONE call site it knows about. This gate guards the *surface*: any future
# `log.Printf("%v", err)` anywhere on the boot path that carries a credential fails here.
#
# WHY IT BOOTS THE BINARY INSTEAD OF GREPPING SOURCE. A source grep for `log.Printf(.*token)` is a
# blocklist, and the leak it needed to catch had no credential-looking token in the source at all —
# the password arrived inside an error value. Only running the thing shows what it actually prints.
#
# SCOPE, STATED HONESTLY. This covers the BOOT path: config load, secrets load, the store open, the
# API wiring, the listen line, and shutdown. It does NOT cover per-request logging — M1 has no
# compose stack to drive live traffic against (fix-plan item 15 defers that), so a request-path leak
# scan has to wait for it. That is a real gap, not a claim of full coverage. It also cannot see a
# credential a caller supplies at request time (a bearer header), only ones configured at boot.
# The precedent is coldmail-send-byo's tests/harness/checks/no_secret_leak.sh, which greps live
# service logs; this is the same idea narrowed to what M1 can actually stand up today.
#
# THE ZEROBOUNCE CANARY. The vendor client is built (buildExternal) only AFTER the store opens, and this
# fixture's store never opens, so the boot path below never reaches the point where the key is read into
# the client: this gate proves that configuring the key (with the fallback switches on) makes nothing
# in config, secrets or wiring print it. What the client itself does with the key (formatting, errors,
# stored state, logs) is proved by TestZB_KeyNeverLeaks_* and TestZB_Stage_KeyNeverInStoredStateOrLogs
# in the Go unit suite, not here: stated so nobody reads this canary as covering the client.
#
# The DSN canary is deliberately carried as a QUERY PARAMETER (`?password=`). That is the shape pgx
# fails to redact, so the gate exercises the worst case rather than the one the driver handles.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$HERE/control-plane"

BIN="$(mktemp -d)/control-plane"
LOG="$(mktemp)"
trap 'rm -rf "$(dirname "$BIN")" "$LOG"' EXIT

go build -o "$BIN" ./cmd/control-plane

# One DISTINCT canary per secret. Distinct so a failure names WHICH credential leaked; a shared
# value would make every leak look the same and hide the second one behind the first.
# 16+ hex-ish chars: long enough not to collide with anything a log line says incidentally.
CANARY_PEPPER="canaryPEPPER00000001"
CANARY_DSNPW="canaryDSNPASSWORD002"
CANARY_FRONTDOOR="canaryFRONTDOOR00003"
CANARY_NODETOKEN="canaryNODETOKEN00004"
CANARY_SUPABASE="canarySUPABASEKEY005"
CANARY_VENDOR="canaryVENDORKEY00006"
CANARY_ZEROBOUNCE="canaryZEROBOUNCEKEY09"   # the ZeroBounce key, read from the variable TM_EXTERNAL_ZEROBOUNCE_KEY_ENV names (default ZEROBOUNCE_API_KEY)
CANARY_QUEUEKEY="canaryQUEUEADDRKEY07"
CANARY_QUEUEPREV="canaryQUEUEPREVKEY08"

# Port 1 on loopback for the DSN host: refused instantly, no DNS, no hang — and the malformed
# pool_max_conns forces the PARSE-stage error, which is the stage whose message carries the DSN.
# A reachable DB would take the connect path and never produce the leaky message.
#
# TM_ENV is left at the default (not "production") on purpose: production requires core secrets and
# these canaries are not real key material (the pepper is not valid hex/base64 of 32 bytes). The boot
# path we need to observe — config, secrets, OpenPG, wiring, listen — runs either way.
set +e
env \
  TM_LISTEN_ADDR="127.0.0.1:0" \
  TM_DATABASE_URL="postgres://cpuser@127.0.0.1:1/cpdb?password=${CANARY_DSNPW}&pool_max_conns=notanumber" \
  TM_PEPPER="$CANARY_PEPPER" \
  TM_FRONTDOOR_TOKEN="$CANARY_FRONTDOOR" \
  TM_NODE_TOKENS="node-a=${CANARY_NODETOKEN}" \
  TM_SUPABASE_KEY="$CANARY_SUPABASE" \
  TM_VENDOR_KEYS="millionverifier=${CANARY_VENDOR}" \
  ZEROBOUNCE_API_KEY="$CANARY_ZEROBOUNCE" \
  TM_FALLBACK_MODE="enabled" \
  TM_EXTERNAL_DPA_CONFIRMED="true" \
  TM_EXTERNAL_DAILY_SPEND_CAP="10" \
  TM_QUEUE_ADDR_KEY="$CANARY_QUEUEKEY" \
  TM_QUEUE_ADDR_KEY_PREV="$CANARY_QUEUEPREV" \
  "$BIN" >"$LOG" 2>&1 &
pid=$!
# Give it time to load config, load secrets, fail the store open, and reach "listening".
for _ in $(seq 1 40); do
  grep -q "listening on" "$LOG" && break
  kill -0 "$pid" 2>/dev/null || break
  sleep 0.25
done
kill -TERM "$pid" 2>/dev/null
wait "$pid" 2>/dev/null
set -e

# ANTI-VACUITY: an empty or truncated log passes every grep below. A gate that greps nothing and
# reports OK is exactly the failure class this gate was added to catch, so assert the boot actually
# happened before trusting the absence of canaries.
for expect in "control-plane starting" "secrets:" "listening on"; do
  grep -qF "$expect" "$LOG" || {
    echo "no-secret-leak FAIL (vacuous run): the boot log is missing '$expect', so the canary scan"
    echo "  proved nothing. The binary did not reach the surface this gate inspects."
    echo "--- log ---"; cat "$LOG"
    exit 1
  }
done
# And the store open MUST have failed at the PARSE stage specifically — that is the only stage whose
# error text carries the DSN. "store unavailable" alone is NOT sufficient evidence: OpenPG also logs
# it for a connect-stage failure, whose message structurally cannot contain a password. A fixture
# that lost its malformed `pool_max_conns` would still reach a connect error against 127.0.0.1:1 and
# still print "store unavailable" — the gate would scan a log that never had a chance to leak and
# report OK. (Measured: mutation GATE-M3 did exactly that and this gate passed, which is why the
# check below names the parse error rather than the generic wrapper.)
#
# Both spellings are accepted because either is proof the parse stage ran: the pgx text when the
# error is unscrubbed, and our own replacement text when it is scrubbed (the normal, fixed case).
grep -qE 'store unavailable .*(cannot parse|message suppressed: it embedded the DSN)' "$LOG" || {
  echo "no-secret-leak FAIL (vacuous run): the store open did not fail AT THE PARSE STAGE, so the"
  echo "  one message known to embed the DSN was never produced and the canary scan proved nothing."
  echo "  Restore the malformed 'pool_max_conns=notanumber' in the DSN fixture — a connect-stage"
  echo "  failure looks similar in the log but cannot carry a credential."
  echo "--- log ---"; cat "$LOG"
  exit 1
}

fail=0
check() { # check <canary> <human name>
  if grep -qF "$1" "$LOG"; then
    echo "no-secret-leak FAIL: $2 appears in the control-plane boot log."
    fail=1
  fi
}
check "$CANARY_PEPPER"    "the Door-3 PEPPER (TM_PEPPER)"
check "$CANARY_DSNPW"     "the DATABASE PASSWORD (TM_DATABASE_URL query param)"
check "$CANARY_FRONTDOOR" "the front-door bearer (TM_FRONTDOOR_TOKEN)"
check "$CANARY_NODETOKEN" "a verifier-node token (TM_NODE_TOKENS)"
check "$CANARY_SUPABASE"  "the Supabase service-role key (TM_SUPABASE_KEY)"
check "$CANARY_VENDOR"    "the legacy vendor key variable (TM_VENDOR_KEYS: no longer read, must still never be logged)"
check "$CANARY_ZEROBOUNCE" "the ZeroBounce API key (ZEROBOUNCE_API_KEY)"
check "$CANARY_QUEUEKEY"  "the P5 queue at-rest key (TM_QUEUE_ADDR_KEY)"
check "$CANARY_QUEUEPREV" "the rotation-overlap queue key (TM_QUEUE_ADDR_KEY_PREV)"

if [ "$fail" -ne 0 ]; then
  echo "--- boot log ---"; cat "$LOG"
  echo "Fix: never log a credential or an error value that may embed one. See"
  echo "  control-plane/internal/store/postgres.go scrubDSNError for the pattern (scrub where the"
  echo "  error is PRODUCED, not at the log call — a call-site fix protects one caller)."
  exit 1
fi

# The line count is ASSERTED, not merely reported. A count printed next to "OK" invites the reader to
# trust it; if the capture were empty the message would read "OK … (0 lines scanned)" and still exit 0.
# Four is the floor for the boot lines the guards above already required.
lines="$(wc -l <"$LOG" | tr -d ' ')"
if [ "$lines" -lt 4 ]; then
  echo "no-secret-leak FAIL (vacuous run): only $lines log lines were scanned, which is fewer than the"
  echo "  boot path produces. The capture, not the code, is what this run measured."
  exit 1
fi

echo "no-secret-leak OK: 8 canaries, none present in the boot log ($lines lines scanned)."
