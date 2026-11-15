package main

// Wiring tests for queue readiness. internal/api's readyz_queue_test.go proves the HANDLER reports a
// declared queue; these prove the control-plane actually DECLARES it. That is a separate failure mode and
// the one that matters here: delete the `srv.WithQueueStatus(...)` line from one of buildQueue's early
// returns and every api test stays green while /readyz goes back to saying `ready` for a module whose
// job routes answer 503.
//
// THE MISCONFIGURATION THIS PINS. TM_QUEUE_ADDR_KEY unset => buildQueue returns before wiring the
// bulk/job routes => an authenticated caller gets 503 `unavailable` on POST /v1/verify/bulk, while
// /readyz (pepper + DB ping only) answered 200 `ready`. Anything that trusted /readyz sent traffic to a
// module whose job routes could not work. The same shape sits behind buildQueue's other two early
// returns (cipher init failed; drainer init failed).
//
// WHY nil ARGUMENTS ARE LEGITIMATE HERE, and where they stop being so. buildQueue's three failure
// paths return before they use pg, orch, hasher or def (pg is first stored in the Enqueuer AFTER the
// cipher is built; the drainer-init failure is returned before `go drainer.Run`). So the failure tests
// pass nil for them. The SUCCESS path does store pg in the Enqueuer and start `drainer.Run(appCtx)`;
// it is exercised only with an ALREADY-CANCELLED appCtx, for which Run's first statement after its log
// line is `<-ctx.Done()` — it returns before any tick could touch the nil store. Do not "simplify" that
// context to context.Background(): the goroutine would then dereference a nil *store.PG.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/api"
	"github.com/coldmail-verify/control-plane/internal/config"
	"github.com/coldmail-verify/control-plane/internal/hash"
	"github.com/coldmail-verify/control-plane/internal/secrets"
)

// mapSource is a secrets.Source over a literal map, so a test controls exactly which secrets exist and
// never inherits the process environment (which `secrets.EnvSource{}` would).
type mapSource map[string]string

func (m mapSource) Get(k string) (string, bool) { v, ok := m[k]; return v, ok }

// okPinger is a Pinger whose database is always reachable, so a not-ready answer in these tests can
// only be the queue's doing.
type okPinger struct{}

func (okPinger) Ping(context.Context) error { return nil }

// anyKeyAuth authenticates every bearer as a per-caller key. It exists only so the bulk route gets past
// authentication and shows the 503 `unavailable` the unwired queue produces — the symptom /readyz must
// now predict.
type anyKeyAuth struct{}

func (anyKeyAuth) LookupCaller(context.Context, []byte, time.Time) (string, string, bool, error) {
	return "caller-1", "user-1", true, nil
}

const testQueueKeyHex = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f" // 32 bytes, hex

// queueServer builds a server on which EVERYTHING ELSE is ready — a real pepper, a reachable DB, a
// per-caller auth — and returns it with the secrets used. extra adds queue secrets on top.
func queueServer(t *testing.T, extra map[string]string) (*api.Server, *secrets.Secrets) {
	t.Helper()
	src := mapSource{secrets.EnvPepper: "queue-readiness-test-pepper-long-enough"}
	for k, v := range extra {
		src[k] = v
	}
	sec, err := secrets.Load(src, false)
	if err != nil {
		t.Fatalf("secrets.Load: %v", err)
	}
	if !sec.HasPepper() {
		t.Fatal("fixture broken: no pepper loaded, so a not-ready answer would not prove the queue's part")
	}
	srv := api.New(config.Defaults(), sec, nil).WithPinger(okPinger{}).WithCallerAuth(anyKeyAuth{})
	return srv, sec
}

type readyzReply struct {
	code    int
	raw     string
	body    map[string]any
	checks  map[string]bool
	reasons []string
}

func fetchReadyz(t *testing.T, srv *api.Server) readyzReply {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	r := readyzReply{code: rec.Code, raw: rec.Body.String(), checks: map[string]bool{}}
	if err := json.Unmarshal(rec.Body.Bytes(), &r.body); err != nil {
		t.Fatalf("/readyz body is not JSON (%v): %s", err, r.raw)
	}
	chk, ok := r.body["checks"].(map[string]any)
	if !ok {
		t.Fatalf("/readyz has no checks{}: %s", r.raw)
	}
	for k, v := range chk {
		b, ok := v.(bool)
		if !ok {
			t.Fatalf("checks[%q] must be a bool, got %T", k, v)
		}
		r.checks[k] = b
	}
	if raw, ok := r.body["reasons"]; ok {
		arr, ok := raw.([]any)
		if !ok {
			t.Fatalf("`reasons` must be an array, got %T", raw)
		}
		for _, v := range arr {
			s, ok := v.(string)
			if !ok {
				t.Fatalf("`reasons` must hold strings, got %T", v)
			}
			r.reasons = append(r.reasons, s)
		}
	}
	return r
}

// requireDeclaredNothingReady fails unless the reply is the pre-queue "ready" shape: 200, status ready, pepper and db
// true, and NO `queue` check — the state of a server buildQueue has not declared anything on.
func requireDeclaredNothingReady(t *testing.T, r readyzReply, when string) {
	t.Helper()
	if r.code != http.StatusOK || r.body["status"] != "ready" || r.body["ready"] != true {
		t.Fatalf("%s: precondition — a server with pepper + reachable DB and no queue declaration must be ready/200, got %d %s", when, r.code, r.raw)
	}
	if _, present := r.checks["queue"]; present {
		t.Fatalf("%s: precondition — nothing has declared a queue yet, so checks.queue must be absent: %s", when, r.raw)
	}
	if _, present := r.body["reasons"]; present {
		t.Fatalf("%s: precondition — `reasons` must be absent before any declaration: %s", when, r.raw)
	}
}

// bulkAnswer POSTs to the bulk route as an authenticated caller and returns the HTTP status and the
// envelope's error.code ("" when the body is not an error).
func bulkAnswer(t *testing.T, srv *api.Server) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/bulk", strings.NewReader(`{"addresses":["a@example.com"]}`))
	req.Header.Set("Authorization", "Bearer any-per-caller-key")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	return rec.Code, env.Error.Code
}

// cancelledCtx is an appCtx that is already done; see the file comment for why the success-path test
// needs exactly this.
func cancelledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// TestBuildQueue_MissingKeyMakesReadyzNotReady is THE misconfiguration, against the REAL buildQueue.
func TestBuildQueue_MissingKeyMakesReadyzNotReady(t *testing.T) {
	srv, sec := queueServer(t, nil) // NO TM_QUEUE_ADDR_KEY
	if sec.HasQueueAddrKey() {
		t.Fatal("fixture broken: a queue key is loaded, so this would not exercise the missing-key path")
	}

	// CONTROL: the identical server, before buildQueue has run, is ready/200 with no queue check. This
	// is what proves the 503 below is buildQueue's doing and not a fixture that was never ready.
	requireDeclaredNothingReady(t, fetchReadyz(t, srv), "before buildQueue")

	buildQueue(cancelledCtx(), config.Defaults(), sec, nil, nil, nil, nil, srv, "fp")

	r := fetchReadyz(t, srv)
	if r.code != http.StatusServiceUnavailable || r.body["status"] != "not_ready" || r.body["ready"] != false {
		t.Fatalf("a missing %s must make /readyz not_ready/503, got %d %s", secrets.EnvQueueAddrKey, r.code, r.raw)
	}
	if q, present := r.checks["queue"]; !present || q {
		t.Errorf("checks.queue must be present and false, got present=%v value=%v (%s)", present, q, r.raw)
	}
	if !r.checks["pepper"] || !r.checks["db"] {
		t.Errorf("pepper and db must stay true — the queue alone is why this is not ready: %v", r.checks)
	}
	if len(r.reasons) != 1 || !strings.HasPrefix(r.reasons[0], "queue: ") || !strings.Contains(r.reasons[0], secrets.EnvQueueAddrKey) {
		t.Errorf("reasons must be exactly one `queue: ...` entry naming %s, got %q", secrets.EnvQueueAddrKey, r.reasons)
	}

	// And the symptom /readyz now predicts: the job routes really do answer 503 `unavailable` to an
	// authenticated caller. If this ever started succeeding, the not-ready above would be a lie in the
	// other direction.
	if code, ecode := bulkAnswer(t, srv); code != http.StatusServiceUnavailable || ecode != "unavailable" {
		t.Errorf("with the key missing, POST /v1/verify/bulk must answer 503 unavailable (the symptom readiness now reports), got %d %q", code, ecode)
	}
}

// TestBuildQueue_CipherInitFailureMakesReadyzNotReady: a key IS present but is not 32 bytes, so the DR-4
// cipher cannot be built and the bulk endpoint stays unwired. The reason carries the cipher error and
// must NOT carry the key — /readyz is unauthenticated.
func TestBuildQueue_CipherInitFailureMakesReadyzNotReady(t *testing.T) {
	const badKey = "canary-too-short-key-VALUE" // not 32 bytes after decoding
	srv, sec := queueServer(t, map[string]string{secrets.EnvQueueAddrKey: badKey})
	if !sec.HasQueueAddrKey() {
		t.Fatal("fixture broken: the key was not loaded, so this would be the missing-key path")
	}
	requireDeclaredNothingReady(t, fetchReadyz(t, srv), "before buildQueue")

	buildQueue(cancelledCtx(), config.Defaults(), sec, nil, nil, nil, nil, srv, "fp")

	r := fetchReadyz(t, srv)
	if r.code != http.StatusServiceUnavailable || r.body["ready"] != false || r.checks["queue"] {
		t.Fatalf("a cipher-init failure must make /readyz not_ready/503 with checks.queue=false, got %d %s", r.code, r.raw)
	}
	if len(r.reasons) != 1 || !strings.HasPrefix(r.reasons[0], "queue: DR-4 cipher init failed") {
		t.Errorf("reason must name the cipher failure, got %q", r.reasons)
	}
	if strings.Contains(r.raw, badKey) {
		t.Errorf("/readyz is unauthenticated and must never carry the key VALUE: %s", r.raw)
	}
}

// TestBuildQueue_DrainerInitFailureMakesReadyzNotReady: the key is good but the drainer refuses to be
// built (both retry ceilings unset — Validate() would reject this config at boot, buildQueue's own
// guard is the second line). Bulk submissions would enqueue and never drain, which is exactly the
// "looks fine, does nothing" state readiness exists to refuse.
func TestBuildQueue_DrainerInitFailureMakesReadyzNotReady(t *testing.T) {
	srv, sec := queueServer(t, map[string]string{secrets.EnvQueueAddrKey: testQueueKeyHex})
	cfg := config.Defaults()
	cfg.Dials.QueueMaxAttempts = 0
	cfg.Dials.QueueMaxRetryAgeSeconds = 0
	requireDeclaredNothingReady(t, fetchReadyz(t, srv), "before buildQueue")

	buildQueue(cancelledCtx(), cfg, sec, nil, nil, nil, nil, srv, "fp")

	r := fetchReadyz(t, srv)
	if r.code != http.StatusServiceUnavailable || r.body["ready"] != false || r.checks["queue"] {
		t.Fatalf("a drainer-init failure must make /readyz not_ready/503 with checks.queue=false, got %d %s", r.code, r.raw)
	}
	if len(r.reasons) != 1 || !strings.HasPrefix(r.reasons[0], "queue: drainer init failed") || !strings.Contains(r.reasons[0], "would enqueue but not drain") {
		t.Errorf("reason must name the drainer failure and its consequence, got %q", r.reasons)
	}
}

// TestBuildQueue_StartedDrainerMakesReadyzReady is the CONTROL for the three failure tests: the same
// real buildQueue, a good key and a good config, declares ready — so the not-ready answers above are
// the failures' doing and not a buildQueue that declares not-ready unconditionally.
func TestBuildQueue_StartedDrainerMakesReadyzReady(t *testing.T) {
	srv, sec := queueServer(t, map[string]string{secrets.EnvQueueAddrKey: testQueueKeyHex})
	requireDeclaredNothingReady(t, fetchReadyz(t, srv), "before buildQueue")

	buildQueue(cancelledCtx(), config.Defaults(), sec, nil, nil, nil, nil, srv, "fp")

	r := fetchReadyz(t, srv)
	if r.code != http.StatusOK || r.body["status"] != "ready" || r.body["ready"] != true {
		t.Fatalf("a started drainer must leave /readyz ready/200, got %d %s", r.code, r.raw)
	}
	if q, present := r.checks["queue"]; !present || !q {
		t.Errorf("checks.queue must be present and true once the drainer started, got present=%v value=%v", present, q)
	}
	if _, present := r.body["reasons"]; present {
		t.Errorf("`reasons` must be omitted when the queue is ready: %s", r.raw)
	}
}

// TestBuildQueue_ReasonsAreInTheRunbook: the reason strings are what an operator SEES on /readyz, and
// operator-runbook.md §6 tells them what each means. This reads the text from the real buildQueue's
// real answer (not a second copy of the literal here) and requires the runbook to contain it, so
// rewording the code without the doc — or the doc without the code — fails.
func TestBuildQueue_ReasonsAreInTheRunbook(t *testing.T) {
	const runbookPath = "../../../Docs/operator-runbook.md"
	b, err := os.ReadFile(runbookPath)
	if err != nil {
		t.Fatalf("read %s: %v", runbookPath, err)
	}
	doc := string(b)

	// Anti-vacuity: the containment check below must be able to say no.
	if strings.Contains(doc, "queue: this sentence is not in the runbook") {
		t.Fatal("fixture broken: the runbook contains the control string, so containment proves nothing")
	}

	reasonOf := func(extra map[string]string, cfg config.Config) string {
		srv, sec := queueServer(t, extra)
		buildQueue(cancelledCtx(), cfg, sec, nil, nil, nil, nil, srv, "fp")
		r := fetchReadyz(t, srv)
		if len(r.reasons) != 1 {
			t.Fatalf("want exactly one reason, got %q (%s)", r.reasons, r.raw)
		}
		return strings.TrimPrefix(r.reasons[0], "queue: ")
	}

	// 1. The missing key: the WHOLE sentence appears verbatim in the runbook.
	missing := reasonOf(nil, config.Defaults())
	if !strings.Contains(missing, secrets.EnvQueueAddrKey) {
		t.Fatalf("the missing-key reason must name %s, got %q", secrets.EnvQueueAddrKey, missing)
	}
	if !strings.Contains(doc, missing) {
		t.Errorf("Docs/operator-runbook.md must quote the missing-key reason verbatim so an operator can find it:\n  %q", missing)
	}

	// 2 and 3. The other two carry a dynamic error in the middle, so the stable lead-in each is
	// quoted by must be in the runbook.
	cipherReason := reasonOf(map[string]string{secrets.EnvQueueAddrKey: "too-short"}, config.Defaults())
	cfg := config.Defaults()
	cfg.Dials.QueueMaxAttempts, cfg.Dials.QueueMaxRetryAgeSeconds = 0, 0
	drainerReason := reasonOf(map[string]string{secrets.EnvQueueAddrKey: testQueueKeyHex}, cfg)
	for _, tc := range []struct{ name, reason, leadIn string }{
		{"cipher", cipherReason, "DR-4 cipher init failed"},
		{"drainer", drainerReason, "drainer init failed"},
	} {
		if !strings.HasPrefix(tc.reason, tc.leadIn) {
			t.Errorf("%s reason must start with %q, got %q", tc.name, tc.leadIn, tc.reason)
		}
		if !strings.Contains(doc, tc.leadIn) {
			t.Errorf("Docs/operator-runbook.md must mention %q (the start of the %s reason /readyz serves)", tc.leadIn, tc.name)
		}
	}
}

// TestBuildAPI_PathsThatNeverReachBuildQueueAreAlreadyNotReady records, with the REAL buildAPI, the
// claim buildQueue's comment makes: every path that returns before buildQueue is reached leaves the
// pinger unattached, so /readyz is ALREADY not-ready there (db:false), for its own reason, and needs
// no queue declaration. No database is needed — both paths below fail before one is reached. The two
// remaining early returns (policy load, default-policy seed) need a reachable Postgres to provoke and
// are covered by reading buildAPI: WithPinger is attached only after both.
func TestBuildAPI_PathsThatNeverReachBuildQueueAreAlreadyNotReady(t *testing.T) {
	sec, err := secrets.Load(mapSource{
		secrets.EnvPepper:       "queue-readiness-test-pepper-long-enough",
		secrets.EnvQueueAddrKey: testQueueKeyHex, // present, so the queue is NOT why these are not ready
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	pepper := make([]byte, 32)
	for i := range pepper {
		pepper[i] = byte(i + 1)
	}
	realHasher, err := hash.NewHasher(pepper)
	if err != nil {
		t.Fatalf("hasher: %v", err)
	}
	// pool_max_conns=notanumber fails at the PARSE stage inside store.OpenPG: instant, no network.
	const unparseableDSN = "postgres://u@127.0.0.1:1/db?pool_max_conns=notanumber"
	for _, tc := range []struct {
		name   string
		dsn    string
		hasher *hash.Hasher
	}{
		// buildAPI's first guard, `hasher == nil || DatabaseURL == ""`, once per operand. The DSN in the
		// first case is non-empty so ONLY the nil hasher can be what stops it.
		{"no hasher", unparseableDSN, nil},
		{"no database url", "", realHasher},
		{"store open fails", unparseableDSN, realHasher},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.DatabaseURL = tc.dsn
			// A live context, not cancelledCtx(): on these paths buildAPI starts nothing, and a
			// pre-cancelled parent would make OpenPG fail for the WRONG reason (context canceled)
			// instead of the parse error this case is named for.
			srv := buildAPI(context.Background(), cfg, sec, tc.hasher)
			r := fetchReadyz(t, srv)
			if r.code != http.StatusServiceUnavailable || r.body["ready"] != false {
				t.Fatalf("a process that never reached buildQueue must already be not-ready, got %d %s", r.code, r.raw)
			}
			if r.checks["db"] {
				t.Errorf("checks.db must be false on this path (the pinger is attached only at the end of wiring): %s", r.raw)
			}
			if _, present := r.checks["queue"]; present {
				t.Errorf("buildQueue was never reached, so nothing may have declared a queue: %s", r.raw)
			}
		})
	}
}
