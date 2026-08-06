package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/config"
	"github.com/coldmail-verify/control-plane/internal/hash"
	"github.com/coldmail-verify/control-plane/internal/orchestrator"
	"github.com/coldmail-verify/control-plane/internal/policy"
	"github.com/coldmail-verify/control-plane/internal/queue"
	"github.com/coldmail-verify/control-plane/internal/secrets"
	"github.com/coldmail-verify/control-plane/internal/store"
)

// =============================================================================
// Cross-tenant job reads, at the ROUTE level, against REAL Postgres (fix-plan item 2 / D1)
// =============================================================================
//
// WHY THIS FILE HAS TO EXIST, AND WHY IT CANNOT USE THE MEM FAKE.
//
// Every other route test in this package injects a MemStore over the JobReader seam. That is the
// exact reason D1 survived to be found by an external audit instead of by this suite: a tenant
// predicate is a WHERE clause, and *with a fake store there is no SQL for a missing WHERE clause to
// be visible in*. The Mem fake answered `m.jobs[jobID]` and every assertion passed. The leak lived
// in `postgres.go`, in three queries keyed on job_id alone, where no test ever looked.
//
// So this test is deliberately the awkward one: real Postgres, real migrations (0013 included), real
// api_callers rows, real HTTP through Server.Handler(). It is gated on TM_TEST_DATABASE_URL and
// SKIP-honest when unset (test-strategy §4), which CI sets — so in CI it RUNS.
//
// WHAT IT PINS (all four are separate properties, and each has its own mutation below):
//
//	(1) A second tenant reading a job it does not own gets 404 — INDISTINGUISHABLE from the 404 for
//	    an id that does not exist at all. Status, headers, and every byte of the body except the
//	    per-request `request_id`, which is equally random in both and so discloses nothing (see
//	    assertIndistinguishable, which normalizes exactly that one field and asserts BOTH that it is
//	    present and that it still varies). This is the no-existence-disclosure posture the senders
//	    already hold and the fix plan requires ("Return 404, not 403"): a 403 confirms the id is real,
//	    which turns a job id into something worth guessing at. UUIDv7 ids are time-ordered, so
//	    "worth guessing at" is not theoretical.
//	(2) ZERO ROWS leak. Not "a redacted page", not "an empty results array with a real count" —
//	    the owner-scoped query matches nothing, asserted at the store level as well as through HTTP,
//	    because the HTTP 404 and the SQL predicate are two independent barriers and either one alone
//	    would be a single edit away from being the only thing holding.
//	(3) The OWNER can read the same job (200, with the verdicts). Without this the 404s above are
//	    vacuous — a broken fixture, a wrong DSN, or an owner column that is NULL for everyone would
//	    produce two identical 404s and a green test while proving nothing.
//	(4) Ownership comes from the CREDENTIAL, never the body (contract §1.4 clause 6 — M1 is
//	    credential-based, not selector-based). A caller that puts another tenant's user id in the
//	    request body still gets a job owned by ITSELF. Taking an owner from the body would satisfy
//	    (1)-(3) perfectly and reintroduce D1 through the front door.
//
// FAIL-VERIFY — each mutation must turn exactly this file red:
//   - `postgres.go` GetJob: drop `AND owner_user_id=$2` → (1) fails: the cross-tenant status read
//     returns 200 instead of 404.
//   - `postgres.go` ListJobResults: drop `AND j.owner_user_id = $2` from the jobs JOIN → (2) fails:
//     the store-level cross-tenant call returns 2 rows instead of 0.
//   - `postgres.go` JobCompletion: drop `AND owner_user_id=$2` from the counts query → (2) fails on
//     the completion assertion (a cross-tenant caller learns the job's progress).
//   - `api.go` handleJobStatus/handleJobResults: revert to `_, _, ok := s.authenticate(...)` and pass
//     "" as the owner → (3) fails: the OWNER's own read 404s, because "" matches no row (SQL
//     three-valued logic — `owner_user_id = NULL` is NULL, not true). Note the direction: this
//     mutation fails CLOSED, which is the property worth having.
//   - `api.go` handleBulkSubmit: take the owner from `req` instead of from authenticate() → (4)
//     fails: the forged-body job becomes readable by the tenant named in the body.
//   - `0013_job_owner.sql`: make owner_user_id NOT NULL DEFAULT some sentinel → (1) fails, because
//     every job becomes readable by whoever presents that sentinel.

// tenancyPG opens the real store, SKIP-honest when the DSN is unset. Separate from the queue
// package's testPGStore so this package needs no test-only export.
func tenancyPG(t *testing.T) *store.PG {
	t.Helper()
	dsn := os.Getenv("TM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("PENDING: set TM_TEST_DATABASE_URL to a migrated Postgres to exercise cross-tenant job reads (never faked — a fake store has no WHERE clause to be missing)")
	}
	pg, err := store.OpenPG(context.Background(), dsn)
	if err != nil {
		t.Fatalf("OpenPG: %v", err)
	}
	t.Cleanup(pg.Close)
	return pg
}

// seedTenantKey inserts an api_callers row binding a bearer key to (caller_id, user_id). Issuance is
// cp_admin (0010) and this test connects as the owner role, so it INSERTs directly — the same
// pattern TestPGLookupCaller_WindowEnforced uses. The point of the direct INSERT is that the READ
// side still goes through the production LookupCaller path.
func seedTenantKey(t *testing.T, pg *store.PG, rawKey, callerID, userID string) {
	t.Helper()
	ctx := context.Background()
	keySHA := sha(rawKey)
	if _, err := pg.Pool().Exec(ctx,
		`INSERT INTO api_callers (caller_id, key_sha256, not_before, not_after, user_id)
		 VALUES ($1,$2,now() - interval '1 hour', NULL, $3)
		 ON CONFLICT (key_sha256) DO UPDATE SET user_id = EXCLUDED.user_id`,
		callerID, keySHA, userID); err != nil {
		t.Fatalf("seed api_callers (%s): %v — if this is 'column user_id does not exist', migration 0013 has not been applied", callerID, err)
	}
	t.Cleanup(func() {
		_, _ = pg.Pool().Exec(context.Background(), `DELETE FROM api_callers WHERE key_sha256=$1`, keySHA)
	})
}

// tenancyServer wires a Server over the REAL PG store: real Enqueuer (so DR-4 sealing runs), real
// JobReader (so the owner predicates are the ones under test), real per-caller auth.
func tenancyServer(t *testing.T, pg *store.PG, now time.Time) (*Server, *queue.AddrCipher) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Env = "production" // enforce auth; no static token configured, so per-caller keys are the only way in
	sec, _ := secrets.Load(fakeSource{}, false)
	h := mustHasher(t)
	def, _ := policy.ColdEmailDefinition()
	cipher, err := queue.NewAddrCipher(key32(7))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	enq := &queue.Enqueuer{Store: pg, Hasher: h, Cipher: cipher, ConfigFingerprint: "cfg-tenancy", Now: func() time.Time { return now }}
	srv := New(cfg, sec, h).WithVerifier(stubVerifier{}, def).WithCallerAuth(pg).WithBulk(enq, pg, 0)
	srv.now = func() time.Time { return now }
	return srv, cipher
}

// submitAs posts a bulk job with the given bearer and returns the job id. extra is spliced into the
// request body verbatim so case (4) can smuggle a forged owner field in.
func submitAs(t *testing.T, srv *Server, bearer, addr, extra string) string {
	t.Helper()
	body := `{"addresses":["` + addr + `"],"caller_ref":"ref-` + bearer + `"` + extra + `}`
	req := httptest.NewRequest(http.MethodPost, "/v1/verify/bulk", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bearer)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit as %s: want 202, got %d (%s)", bearer, rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("submit as %s: bad JSON: %v", bearer, err)
	}
	id, _ := resp["job_id"].(string)
	if id == "" {
		t.Fatalf("submit as %s: no job_id in %s", bearer, rec.Body.String())
	}
	return id
}

// getAs performs an authenticated GET and returns the recorder (status + headers + exact bytes).
func getAs(t *testing.T, srv *Server, bearer, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// cleanupJob removes a job and everything hanging off it, as the owner role (cp_runtime cannot
// DELETE — 0009/0012 grants). Keeps a shared CI database from accumulating fixtures.
func cleanupJob(t *testing.T, pg *store.PG, jobID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pg.Pool().Exec(ctx, `DELETE FROM work_item_attempts WHERE work_item_id IN (SELECT id FROM work_items WHERE job_id=$1)`, jobID)
		_, _ = pg.Pool().Exec(ctx, `DELETE FROM verifications WHERE id IN (SELECT verification_id FROM work_items WHERE job_id=$1 AND verification_id IS NOT NULL)`, jobID)
		_, _ = pg.Pool().Exec(ctx, `DELETE FROM work_items WHERE job_id=$1`, jobID)
		_, _ = pg.Pool().Exec(ctx, `DELETE FROM jobs WHERE id=$1`, jobID)
	})
}

// TestPGCrossTenantJobRead_404IdenticalAndZeroRows is item 2. See the file header for the four
// properties and the mutation list.
func TestPGCrossTenantJobRead_404IdenticalAndZeroRows(t *testing.T) {
	pg := tenancyPG(t)
	ctx := context.Background()
	now := time.Now().UTC()
	// Unique key material + caller ids per run so repeated runs against a shared CI DB don't collide.
	tag := now.Format("150405.000000000")

	const (
		userAlice = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		userBob   = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
		// A syntactically valid uuid that is not a job. Must be a real uuid: jobs.id is uuid, so a
		// non-uuid path segment would be a query ERROR (500), not the 404 we are comparing against.
		noSuchJob = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	)
	keyAlice := "tenancy-alice-" + tag
	keyBob := "tenancy-bob-" + tag
	seedTenantKey(t, pg, keyAlice, "caller_alice_"+tag, userAlice)
	seedTenantKey(t, pg, keyBob, "caller_bob_"+tag, userBob)

	srv, cipher := tenancyServer(t, pg, now)

	// Alice submits, and we DRAIN so her job actually holds settled verdicts. Draining matters: an
	// undrained job has no rows in the results join, so a "zero rows leaked" assertion would pass
	// against an empty table. The leak we are testing for is verdict PII, so there must be verdict
	// PII present to leak.
	aliceJob := submitAs(t, srv, keyAlice, "alice-"+tag+"@acmecorp.com", "")
	cleanupJob(t, pg, aliceJob)
	drainOnce(t, pg, cipher, now)

	// Sanity + anti-vacuity (property 3): the OWNER can read her own job and its verdicts.
	ownerStatus := getAs(t, srv, keyAlice, "/v1/jobs/"+aliceJob)
	if ownerStatus.Code != http.StatusOK {
		t.Fatalf("owner's own job status: want 200, got %d (%s)", ownerStatus.Code, ownerStatus.Body.String())
	}
	var aliceView map[string]any
	json.Unmarshal(ownerStatus.Body.Bytes(), &aliceView)
	if aliceView["complete"] != true {
		t.Fatalf("fixture not drained (complete=%v) — the leak test needs settled verdicts to be leakable: %s",
			aliceView["complete"], ownerStatus.Body.String())
	}
	ownerResults := getAs(t, srv, keyAlice, "/v1/jobs/"+aliceJob+"/results")
	if ownerResults.Code != http.StatusOK {
		t.Fatalf("owner's own results: want 200, got %d (%s)", ownerResults.Code, ownerResults.Body.String())
	}
	var ownerPage struct {
		Results []map[string]any `json:"results"`
	}
	json.Unmarshal(ownerResults.Body.Bytes(), &ownerPage)
	if len(ownerPage.Results) != 1 {
		t.Fatalf("owner must see her 1 settled verdict, got %d — fixture broken, the 404s below would be vacuous: %s",
			len(ownerPage.Results), ownerResults.Body.String())
	}

	// ---------------------------------------------------------------------
	// Property 1: Bob reading Alice's job is BYTE-IDENTICAL to Bob reading a nonexistent job.
	// ---------------------------------------------------------------------
	crossStatus := getAs(t, srv, keyBob, "/v1/jobs/"+aliceJob)
	absentStatus := getAs(t, srv, keyBob, "/v1/jobs/"+noSuchJob)
	assertIndistinguishable(t, "GET /v1/jobs/{id}", crossStatus, absentStatus)
	if crossStatus.Code != http.StatusNotFound {
		t.Fatalf("D1: cross-tenant job status must be 404, got %d (%s)", crossStatus.Code, crossStatus.Body.String())
	}

	crossResults := getAs(t, srv, keyBob, "/v1/jobs/"+aliceJob+"/results")
	absentResults := getAs(t, srv, keyBob, "/v1/jobs/"+noSuchJob+"/results")
	assertIndistinguishable(t, "GET /v1/jobs/{id}/results", crossResults, absentResults)
	if crossResults.Code != http.StatusNotFound {
		t.Fatalf("D1: cross-tenant job RESULTS must be 404, got %d (%s)", crossResults.Code, crossResults.Body.String())
	}
	// Nothing of Alice's may appear in what Bob got back, on either route.
	for _, rec := range []*httptest.ResponseRecorder{crossStatus, crossResults} {
		for _, leak := range []string{"acmecorp.com", "ref-" + keyAlice, aliceJob, userAlice} {
			if bytes.Contains(rec.Body.Bytes(), []byte(leak)) {
				t.Fatalf("cross-tenant response leaked %q: %s", leak, rec.Body.String())
			}
		}
	}

	// ---------------------------------------------------------------------
	// Property 2: ZERO ROWS at the STORE level — the second, independent barrier. The handler 404s
	// above prove the response shape; these prove the QUERY, which is where D1 actually lived.
	// ---------------------------------------------------------------------
	if rows, next, err := pg.ListJobResults(ctx, aliceJob, userBob, "", 100); err != nil {
		t.Fatalf("cross-tenant ListJobResults err: %v", err)
	} else if len(rows) != 0 || next != "" {
		t.Fatalf("D1: cross-tenant ListJobResults returned %d row(s) (next=%q) — want 0; the owner JOIN is not filtering", len(rows), next)
	}
	if _, known, err := pg.GetJob(ctx, aliceJob, userBob); err != nil {
		t.Fatalf("cross-tenant GetJob err: %v", err)
	} else if known {
		t.Fatal("D1: cross-tenant GetJob reported the job as known — the owner predicate is not filtering")
	}
	// Completion is a smaller signal than the verdicts, but it is still Alice's: it says how far
	// along her job is. A cross-tenant caller must not learn it either.
	if _, _, _, err := pg.JobCompletion(ctx, aliceJob, userBob); err == nil {
		t.Fatal("D1: cross-tenant JobCompletion succeeded — want ErrNotWired (mapped to the same 404)")
	}
	// And the owner-scoped call to the SAME method DOES work — so the error above is about tenancy,
	// not about the method being broken for everyone.
	if complete, total, _, err := pg.JobCompletion(ctx, aliceJob, userAlice); err != nil || !complete || total != 1 {
		t.Fatalf("owner JobCompletion: complete=%v total=%d err=%v (want true/1/nil)", complete, total, err)
	}

	// ---------------------------------------------------------------------
	// Property 4: the owner comes from the CREDENTIAL, never the body (contract §1.4 clause 6).
	// Bob submits while naming ALICE in the body, every way the field might plausibly be spelled.
	// The job must come out owned by BOB.
	// ---------------------------------------------------------------------
	forged := submitAs(t, srv, keyBob, "bob-"+tag+"@acmecorp.com",
		`,"user_id":"`+userAlice+`","owner_user_id":"`+userAlice+`","owner":"`+userAlice+`"`)
	cleanupJob(t, pg, forged)

	if row, known, err := pg.GetJob(ctx, forged, userBob); err != nil || !known {
		t.Fatalf("forged-body job must belong to the AUTHENTICATED submitter (Bob): known=%v err=%v", known, err)
	} else if row.OwnerUserID != userBob {
		t.Fatalf("owner came from the BODY, not the credential: owner=%q want %q — this is D1 through the front door", row.OwnerUserID, userBob)
	}
	if _, known, err := pg.GetJob(ctx, forged, userAlice); err != nil {
		t.Fatalf("forged-body cross-check err: %v", err)
	} else if known {
		t.Fatal("a body-supplied user_id assigned ownership — a caller can hand its jobs to (or read them as) another tenant")
	}
	if got := getAs(t, srv, keyAlice, "/v1/jobs/"+forged); got.Code != http.StatusNotFound {
		t.Fatalf("Alice reading the job Bob forged in her name: want 404, got %d (%s)", got.Code, got.Body.String())
	}
}

// TestPGOwnerlessJobIsReadClosed pins the 0013 backfill decision: a key with NO platform user
// (user_id NULL — every pre-0013 key, and any key issued without one) can still SUBMIT, but the
// resulting job is OWNERLESS and readable by NOBODY, including the caller that created it.
//
// This is the deliberate choice recorded in 0013: no sentinel owner. A sentinel would assert an
// owner the system never knew, and would make every legacy job readable by whoever presents it.
// Fail-CLOSED on read is the only honest answer for a row whose owner is unrecoverable.
//
// FAIL-VERIFY: give owner_user_id a NOT NULL DEFAULT (or make MemStore/PG treat "" as a wildcard) →
// the second assertion starts returning 200 and this test fails.
func TestPGOwnerlessJobIsReadClosed(t *testing.T) {
	pg := tenancyPG(t)
	now := time.Now().UTC()
	tag := now.Format("150405.000000000")

	key := "tenancy-nouser-" + tag
	// user_id NULL: models a pre-0013 key.
	keySHA := sha(key)
	if _, err := pg.Pool().Exec(context.Background(),
		`INSERT INTO api_callers (caller_id, key_sha256, not_before, not_after, user_id)
		 VALUES ($1,$2,now() - interval '1 hour', NULL, NULL)
		 ON CONFLICT (key_sha256) DO UPDATE SET user_id = NULL`,
		"caller_nouser_"+tag, keySHA); err != nil {
		t.Fatalf("seed userless caller: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pg.Pool().Exec(context.Background(), `DELETE FROM api_callers WHERE key_sha256=$1`, keySHA)
	})

	srv, _ := tenancyServer(t, pg, now)

	// It authenticates and submits — the front door is unchanged for a legacy key (no new 401s).
	jobID := submitAs(t, srv, key, "nouser-"+tag+"@acmecorp.com", "")
	cleanupJob(t, pg, jobID)

	// But the job it just created is unreadable, by itself or anyone.
	if got := getAs(t, srv, key, "/v1/jobs/"+jobID); got.Code != http.StatusNotFound {
		t.Fatalf("an ownerless job must be read-CLOSED: want 404, got %d (%s)", got.Code, got.Body.String())
	}
	if got := getAs(t, srv, key, "/v1/jobs/"+jobID+"/results"); got.Code != http.StatusNotFound {
		t.Fatalf("an ownerless job's results must be read-CLOSED: want 404, got %d (%s)", got.Code, got.Body.String())
	}
	// The row IS there — so the 404 is the owner predicate refusing, not a failed submit.
	var stored int
	if err := pg.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM jobs WHERE id=$1 AND owner_user_id IS NULL`, jobID).Scan(&stored); err != nil {
		t.Fatalf("count ownerless job: %v", err)
	}
	if stored != 1 {
		t.Fatalf("expected exactly 1 ownerless job row for %s, found %d — the 404 above may be a failed submit, not a refused read", jobID, stored)
	}
}

// assertIndistinguishable is the anti-existence-disclosure control: two responses must be the same
// status, the same content type, and the same bytes. Byte equality is the bar the fix plan sets —
// a differing body length is itself an oracle ("job not found" vs "not authorized" leaks the
// distinction even at equal status codes).
func assertIndistinguishable(t *testing.T, route string, cross, absent *httptest.ResponseRecorder) {
	t.Helper()
	if cross.Code != absent.Code {
		t.Fatalf("%s: cross-tenant status %d != nonexistent-id status %d — the difference IS the disclosure",
			route, cross.Code, absent.Code)
	}
	if ct, at := cross.Header().Get("Content-Type"), absent.Header().Get("Content-Type"); ct != at {
		t.Fatalf("%s: content-type differs: cross=%q absent=%q", route, ct, at)
	}
	// request_id is EXCLUDED from the comparison, and excluding it is the whole subtlety here.
	//
	// This helper compared raw bytes when it was written, and that was correct then. Item 18 (commit
	// cc3ce0e) later routed every error body through writeErr, which stamps a FRESH uuid into
	// `request_id` per request — deliberately, so an operator can correlate one body with one log
	// line. Two separate HTTP requests can therefore never be byte-equal again, which made this
	// assertion IMPOSSIBLE TO PASS rather than merely wrong. It went unnoticed for two reasons at
	// once, and both are the reason the exclusion is spelled out rather than done quietly:
	//   (a) this whole file t.Skip()s unless TM_TEST_DATABASE_URL is set, so an ordinary local run
	//       never executes it; and
	//   (b) this repo has NO git remote, so .github/workflows/ci.yml — which DOES set that DSN and
	//       would have gone red on the first push — has never actually run.
	// A later commit invalidated an older assertion and the only two things that would have noticed
	// were both silent. Found 2026-08-06 by running the suite against a real Postgres by hand.
	//
	// THE DISCLOSURE PROPERTY IS UNCHANGED, and that is why the fix is to normalize rather than to
	// weaken. What must not differ is anything DERIVED FROM WHETHER THE JOB EXISTS. request_id is
	// derived from the request, not from the answer: it is equally random in both responses, carries
	// no information about the id being real, and would be identical in distribution if the job
	// existed or not. Every other byte — code, message, details, field order, content-type, status —
	// is still compared exactly, so a 403-vs-404, a "job not owned by you" message, a `details`
	// entry naming the owner, or an extra field on one branch all still fail this.
	//
	// The normalization is deliberately NARROW: exactly the value of the top-level `request_id`
	// field, replaced with a fixed sentinel. It is NOT a "strip all uuids" pass — that would also
	// blank a leaked job id or owner id, which is the very leak assertions below hunt for. If a
	// SECOND nondeterministic field is ever added to the envelope, this must fail loudly and be
	// extended one field at a time, rather than being loosened into a subset match.
	crossN, absentN := normalizeErrRequestID(t, cross.Body.Bytes()), normalizeErrRequestID(t, absent.Body.Bytes())
	if !bytes.Equal(crossN, absentN) {
		t.Fatalf("%s: bodies differ in something OTHER than request_id — the difference IS the disclosure:\n  cross-tenant : %s\n  nonexistent  : %s\n  (raw)\n  cross-tenant : %s\n  nonexistent  : %s",
			route, crossN, absentN, cross.Body.String(), absent.Body.String())
	}
	// Anti-vacuity for the normalization itself: if request_id ever stops being present, or stops
	// being per-request, the comparison above silently becomes weaker than advertised (two bodies
	// that are equal because both had nothing to normalize). Both halves are asserted.
	if !bytes.Contains(cross.Body.Bytes(), []byte(`"request_id"`)) {
		t.Fatalf("%s: error body carries no request_id field, so normalizing it proves nothing — either the envelope changed (update this helper) or writeErr regressed: %s",
			route, cross.Body.String())
	}
	if bytes.Equal(cross.Body.Bytes(), absent.Body.Bytes()) {
		t.Fatalf("%s: the two error bodies are byte-identical INCLUDING request_id — that id is supposed to be minted per request (item 18). If it has become constant, the log-correlation mechanism is broken: %s",
			route, cross.Body.String())
	}
}

// normalizeErrRequestID replaces the value of the error envelope's top-level `request_id` with a
// fixed sentinel, leaving every other byte — including key order, which json.Marshal emits in struct
// field order and is therefore stable — untouched.
//
// Implemented by editing the RAW BYTES rather than by unmarshal/marshal round-tripping through
// map[string]any, on purpose: a round-trip through a map would reorder keys, drop an unexpected
// field entirely, and coerce number formats, so it would hide exactly the envelope-shape differences
// this comparison exists to catch. Here, a field the helper does not know about still shows up as a
// byte difference.
func normalizeErrRequestID(t *testing.T, body []byte) []byte {
	t.Helper()
	return errRequestIDValue.ReplaceAll(body, []byte(`"request_id":"<normalized>"`))
}

// The `[^"]*` value class is deliberate: it matches whatever writeErr put there (a UUIDv7, or the
// obviously-degenerate all-zero fallback) without asserting a uuid SHAPE. This helper's job is to
// remove a known-nondeterministic value from a comparison, not to validate it — the id's shape is
// pinned in errors_test.go, where it belongs.
var errRequestIDValue = regexp.MustCompile(`"request_id":"[^"]*"`)

// drainOnce runs one real drainer tick against real Postgres so the fixture job holds settled
// verdicts. Uses the production drainer + CAS + settle path (nothing about the settlement is faked);
// only the verdict COMPUTATION is stubbed, because this test is about tenancy, not about SMTP.
func drainOnce(t *testing.T, pg *store.PG, cipher *queue.AddrCipher, now time.Time) {
	t.Helper()
	dr, err := queue.NewDrainer(queue.Deps{
		Store: pg, Compute: pgSettleComputer{now: now}, Policy: fixedPolicy{}, Cipher: cipher,
		Backoff:  queue.Backoff{Base: time.Minute, Factor: 2, Cap: time.Hour, RateWait: time.Second},
		Now:      func() time.Time { return now },
		WorkerID: "tenancy-drainer", LeaseTTL: time.Minute, MaxSMTPDeadline: 10 * time.Second,
		MaxAttempts: 3, MaxRetryAge: time.Hour, BatchSize: 50, ReclaimEvery: time.Hour,
	})
	if err != nil {
		t.Fatalf("drainer: %v", err)
	}
	dr.TickOnce(context.Background())
}

// pgSettleComputer settles every item valid/accepted on the first attempt with a REAL uuid (the
// package's MemStore-only settleComputer emits a short id that Postgres would reject).
type pgSettleComputer struct{ now time.Time }

func (c pgSettleComputer) ComputeForQueue(_ context.Context, _ string, _ *policy.Definition, _ string) (orchestrator.QueueOutcome, error) {
	id := tenancyVerificationID()
	return orchestrator.QueueOutcome{Class: orchestrator.QSettled, Row: store.VerificationRow{
		ID: id, RequestID: id, CheckedAt: c.now, CreatedAt: c.now,
		EmailHash: []byte("tenancy-hash"), HashScheme: hash.SchemeTag, Domain: "acmecorp.com",
		Status: "valid", Reason: "accepted", Confidence: 90, Recommendation: "send",
		PolicyKey: "cold-email", PolicyVersion: 1, APIVersion: "v1", ConfigFingerprint: "cfg-tenancy",
	}}, nil
}

func (c pgSettleComputer) EscalateExhausted(_ context.Context, _ string, _ *policy.Definition, out orchestrator.QueueOutcome, _, _ string) orchestrator.QueueOutcome {
	return out
}

// tenancyVerificationID mints the verification-row id this file's fixtures settle under.
//
// WHY THIS IS NOT A COUNTER. It was one, and the counter was a real defect that this suite's own
// first full-repo run caught. `verifications` is APPEND-ONLY (Door 2 — the insert has no upsert and
// no ON CONFLICT), its primary key is `id`, and `go test ./...` points EVERY package at the one
// TM_TEST_DATABASE_URL database. internal/queue/pg_e2e_test.go has held a `randHex12()` since
// before this file existed: same counter-from-1 algorithm, same "018f0000-0000-7000-8000-" prefix.
// Two packages therefore minted the identical id sequence into the identical table, and the second
// one to settle lost the insert to a duplicate-key error. The drainer logs that and moves on, so
// the visible failure was not "duplicate key" — it was this file's own anti-vacuity guard at
// property (3) reporting `complete=false`, i.e. a leak test with nothing left to leak.
//
// It reproduced 5/5 on `go test ./internal/api/ ./internal/queue/` and passed 5/5 on either package
// alone: the class of flake that looks like an infrastructure hiccup and gets re-run rather than
// read. CI runs `go test ./...` against one Postgres service, so it was live there too.
//
// crypto/rand rather than a wider counter or a PID/time prefix: the id only has to be unique, never
// reproducible, and 12 hex chars of real entropy per row cannot collide with a sibling package's
// scheme no matter what that scheme is or when it was written. There is nothing left to coordinate.
func tenancyVerificationID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic("tenancy fixture: crypto/rand unavailable: " + err.Error())
	}
	return "018f0000-0000-7000-8000-" + hex.EncodeToString(b)
}

// TestPGTenancyFixtureIDsDoNotCollideWithSiblingPackages is the CONTROL for the fix above, and it is
// the test the original counter would have failed.
//
// The property: this file's fixture ids must be unique per call and must not be predictable from a
// call ordinal. A counter satisfies "unique per call" perfectly WITHIN one process and still
// collides across packages, which is exactly how the defect hid — so asserting uniqueness alone is
// not enough. This asserts the stronger property that actually prevents the collision: two
// independent id sequences started from the same state do not agree.
//
// Fail-verify (observed, not predicted): restore `tenancyVerificationID` to the counter body and
// this test fails at the collision assertion, because two "runs" of a counter reset to 0 emit the
// identical sequence. Both sub-assertions are load-bearing — the uniqueness half alone passes
// against the counter.
//
// It needs no database: the bug was in id GENERATION, so generation is what gets pinned. Pinning it
// here rather than in the drain path also means the guard survives someone rewriting the fixtures.
func TestPGTenancyFixtureIDsDoNotCollideWithSiblingPackages(t *testing.T) {
	const n = 512

	// (a) Unique per call.
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		id := tenancyVerificationID()
		if seen[id] {
			t.Fatalf("tenancyVerificationID repeated %q within %d calls — verifications.id is a "+
				"primary key on an append-only table, so a repeat is a lost INSERT", id, n)
		}
		seen[id] = true
		// Must stay a syntactically valid uuid or Postgres rejects the row outright.
		if len(id) != 36 {
			t.Fatalf("id %q is %d chars, want 36 (uuid) — a malformed id fails as a 500, not a 404", id, len(id))
		}
	}

	// (b) NOT derivable from call order. A counter passes (a) and fails (b); crypto/rand passes both.
	// internal/queue/pg_e2e_test.go's randHex12() is the concrete sibling this guards against: same
	// prefix, same counter algorithm, same table, different package.
	const counterPrefix = "018f0000-0000-7000-8000-"
	ordinal := func(i int) string {
		const hexd = "0123456789abcdef"
		nn := i
		b := make([]byte, 12)
		for j := 11; j >= 0; j-- {
			b[j] = hexd[nn&0xf]
			nn >>= 4
		}
		return counterPrefix + string(b)
	}
	for i := 1; i <= n; i++ {
		if seen[ordinal(i)] {
			t.Fatalf("id %q is the %d-th value of a from-1 counter and this fixture minted it: the "+
				"ids are call-order derivable, so any sibling package using the same scheme "+
				"(internal/queue/pg_e2e_test.go does) collides on verifications.id under "+
				"`go test ./...`, which shares one TM_TEST_DATABASE_URL", ordinal(i), i)
		}
	}
}
