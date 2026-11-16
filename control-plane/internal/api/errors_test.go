package api

// errors_test.go — the mechanisms for fix-plan item 18 (audit D3, classifications 2.1-2.4/2.7,
// UI questions 5-7, canonical-module-contract.md §1.3).
//
// WHAT THESE HOLD. errors.go is a serializer plus two lookup maps; on its own it proves nothing,
// because the defect item 18 fixes was never "no serializer exists" — it was 26 hand-rolled call
// sites, each free to answer in its own shape. A single site that keeps hand-rolling reproduces the
// whole defect, and the serializer's existence is exactly what would make that invisible in review.
// So the load-bearing test here is not any one envelope assertion: it is
// TestErrorEnvelope_NoHandRolledErrorBodiesRemain, which reads the SOURCE of every non-test file in
// this package and fails if a raw error body reappears in any of them. Everything else pins the
// properties of the envelope that a source scan cannot see.
//
// EACH TEST NAMES ITS OWN CONTROL. A test that only ever sees the current code pass is a test whose
// failure mode is unknown; the break each one is meant to catch is written above it, and every one
// was run. That discipline earned its keep here: the source scan originally read api.go ALONE, and
// breaking it by moving a handler into a new file showed the scan stayed green while the guarded
// property was gone. It globs the package now — see that test's own header.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/config"
	"github.com/coldmail-verify/control-plane/internal/policy"
	"github.com/coldmail-verify/control-plane/internal/queue"
	"github.com/coldmail-verify/control-plane/internal/secrets"
	"github.com/coldmail-verify/control-plane/internal/store"
)

// bareUUID is the request_id shape M1 must mint: a bare UUID, NOT the `req_<hex>` form audit D23
// deleted from this package. The distinction is not cosmetic — the LOG's request_id column is
// Postgres `uuid` (§3.2.2), so a prefixed id could never be persisted, and an id that cannot reach
// the log cannot correlate anything. This regex is what makes "M1 does not copy M3's minter" a
// mechanism rather than a comment.
var bareUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// splitRequestID decodes an error envelope, pulls out request_id, and returns the body with that
// field BLANKED plus the id itself. Two error bodies can never be byte-identical once each carries
// its own correlation id, so comparing "everything but the id" is what lets a test still assert two
// responses are otherwise the same shape (frontdoor_defer_test.go's sub-mode indistinguishability).
func splitRequestID(t *testing.T, raw []byte) (blanked, requestID string) {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("error body is not JSON: %v (%s)", err, raw)
	}
	body, ok := env["error"].(map[string]any)
	if !ok {
		t.Fatalf("error body has no `error` object — every error in this package goes through writeErr, so this is either a hand-rolled site or a shape change: %s", raw)
	}
	requestID, _ = body["request_id"].(string)
	body["request_id"] = ""
	out, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	return string(out), requestID
}

// requireEnvelope asserts the §1.3 envelope on ONE response: the `error` object, a non-empty code
// from the registered set, the code's curated message (never an interpolated one), and a bare-UUID
// request_id. Returns the decoded body so a caller can make its own extra assertions.
func requireEnvelope(t *testing.T, rec *httptest.ResponseRecorder, wantCode string, wantStatus int) map[string]any {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status=%d, want %d (body %s)", rec.Code, wantStatus, rec.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("error body is not JSON: %v (%s)", err, rec.Body.String())
	}
	// The OLD shape was a top-level `{"error":"<free text>"}` — a STRING, not an object. Asserting
	// the type is what distinguishes "converted" from "still hand-rolled": a leftover site
	// deserializes into this same map with error holding a string, and would otherwise slip through.
	body, ok := env["error"].(map[string]any)
	if !ok {
		t.Fatalf("`error` is not an object — the pre-item-18 shape was `{\"error\":\"<free text>\"}`; this site was not converted: %s", rec.Body.String())
	}
	if got, _ := body["code"].(string); got != wantCode {
		t.Fatalf("error.code=%q, want %q: %s", got, wantCode, rec.Body.String())
	}
	// The message must be the CURATED constant for that code. This is the two-vocabulary rule as a
	// mechanism: if a call site ever interpolates a reason into the caller-facing sentence, the
	// message stops matching fixedMessages and this fails.
	wantMsg := fixedMessages[wantCode]
	if wantMsg == "" {
		t.Fatalf("test bug: code %q has no fixedMessages entry", wantCode)
	}
	if got, _ := body["message"].(string); got != wantMsg {
		t.Fatalf("error.message=%q, want the curated constant %q — a message that differs means a call site interpolated its own text (two-vocabulary rule): %s",
			got, wantMsg, rec.Body.String())
	}
	id, _ := body["request_id"].(string)
	if id == "" {
		t.Fatalf("error carries no request_id — item 18's whole purpose is that a user-reported failure is correlatable: %s", rec.Body.String())
	}
	if !bareUUID.MatchString(id) {
		t.Fatalf("request_id=%q is not a bare UUID. M3's platformapi mints `req_<hex>` and that is correct FOR M3, but the LOG's request_id column here is Postgres `uuid` (§3.2.2) — a prefixed id can never be persisted. Audit D23 deleted exactly this from helpers.go; do not reinstate it.", id)
	}
	return body
}

// unwiredServer is a Server with NO verifier, NO store, NO pepper — the honest not-ready state.
// Distinct from api_test.go's helpers because this one needs a server whose EVERY route errors, so
// the envelope can be checked across surfaces without arranging a separate failure per route.
func unwiredServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.Defaults() // development => open auth, so a 503 is not masked by a 401
	sec, err := secrets.Load(fakeSource{}, false)
	if err != nil {
		t.Fatalf("secrets: %v", err)
	}
	return New(cfg, sec, nil)
}

// TestErrorEnvelope_EveryErrorSurfaceCarriesCodeMessageRequestID walks the error surfaces reachable
// without a DB and holds the §1.3 envelope on each. Four DIFFERENT codes over three routes and two
// server wirings, because a single route would not distinguish "the envelope is right" from "this
// one handler was converted."
//
// FAILS IF: any of these sites goes back to `writeJSON(w, status, map[string]any{"error": ...})`;
// request_id is dropped from errorBody; a code is renamed without its fixedMessages entry.
func TestErrorEnvelope_EveryErrorSurfaceCarriesCodeMessageRequestID(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)

	t.Run("validation_failed on a bodyless verify", func(t *testing.T) {
		srv := wiredServer(t)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/verify", strings.NewReader(`{}`)))
		body := requireEnvelope(t, rec, codeValidationFailed, http.StatusBadRequest)
		// details[] must name the offending field with a FIXED slug, never echoed input.
		requireDetail(t, body, "email", issueRequired)
	})

	t.Run("unavailable on an unwired verify", func(t *testing.T) {
		srv := unwiredServer(t)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/verify", strings.NewReader(`{"email":"jane@acmecorp.com"}`)))
		requireEnvelope(t, rec, codeUnavailable, http.StatusServiceUnavailable)
	})

	t.Run("unauthenticated on bulk with no key", func(t *testing.T) {
		srv, _, _ := bulkServer(t, now)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/verify/bulk",
			strings.NewReader(`{"addresses":["a@acmecorp.com"]}`)))
		requireEnvelope(t, rec, codeUnauthenticated, http.StatusUnauthorized)
	})

	t.Run("not_found on someone else's job", func(t *testing.T) {
		srv, _, _ := bulkServer(t, now)
		r := httptest.NewRequest(http.MethodGet, "/v1/jobs/019fd000-0000-7000-8000-000000000000", nil)
		r.Header.Set("Authorization", "Bearer bulk-key")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, r)
		requireEnvelope(t, rec, codeNotFound, http.StatusNotFound)
	})

	t.Run("paused on a Rung-4 verify", func(t *testing.T) {
		srv, _, _ := deferServer(t, now, config.FrontDoorReject)
		rec := do(t, srv, http.MethodPost, "/v1/verify", `{"email":"jane@acmecorp.com"}`)
		requireEnvelope(t, rec, codePaused, http.StatusServiceUnavailable)
	})
}

// requireDetail asserts details[] contains exactly the given field/issue pair, and that the issue is
// from the FIXED slug set. A free-text issue would be a second free-text channel — the thing the
// envelope replaced — arriving through the field meant to prevent it.
func requireDetail(t *testing.T, body map[string]any, field, issue string) {
	t.Helper()
	raw, ok := body["details"].([]any)
	if !ok || len(raw) == 0 {
		t.Fatalf("error carries no details[]; want field=%q issue=%q: %+v", field, issue, body)
	}
	allowed := map[string]bool{issueRequired: true, issueMalformed: true, issueOutOfRange: true, issueNotFound: true, issueConflict: true, issueDuplicate: true}
	for _, d := range raw {
		m, ok := d.(map[string]any)
		if !ok {
			t.Fatalf("details[] entry is not an object: %+v", d)
		}
		gotIssue, _ := m["issue"].(string)
		if !allowed[gotIssue] {
			t.Fatalf("details[].issue=%q is not one of the fixed slugs (%v) — an unlisted issue is free text arriving through the field that exists to prevent it", gotIssue, allowed)
		}
		if m["field"] == field && gotIssue == issue {
			return
		}
	}
	t.Fatalf("details[] has no {field:%q, issue:%q}: %+v", field, issue, raw)
}

// TestErrorEnvelope_UnavailableAndPausedAreDistinctCodesOverOneStatus — the item's central design
// decision, held by a test rather than described by a comment.
//
// Both conditions answer 503, and before item 18 both answered the same free-text body ("not ready"
// vs "service paused" — distinguishable only by prose a client would have to string-match). They are
// operationally OPPOSITE: `unavailable` means this deployment is not wired (retry elsewhere, page
// someone), `paused` means an operator pulled Rung 4 deliberately (retry later, do NOT page). A
// client that cannot tell them apart cannot behave correctly for either.
//
// FAILS IF: anyone "simplifies" codeUnavailable and codePaused into one code because they share a
// status — which is the natural-looking cleanup, and the reason this test exists.
func TestErrorEnvelope_UnavailableAndPausedAreDistinctCodesOverOneStatus(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	const body = `{"email":"jane@acmecorp.com"}`

	unwired := httptest.NewRecorder()
	unwiredServer(t).Handler().ServeHTTP(unwired, httptest.NewRequest(http.MethodPost, "/v1/verify", strings.NewReader(body)))
	notReady := requireEnvelope(t, unwired, codeUnavailable, http.StatusServiceUnavailable)

	srvPaused, _, _ := deferServer(t, now, config.FrontDoorReject)
	paused := requireEnvelope(t, do(t, srvPaused, http.MethodPost, "/v1/verify", body), codePaused, http.StatusServiceUnavailable)

	// The assertion that matters: same status, DIFFERENT code, and different curated message too —
	// so the distinction survives whether a client branches on the code or a human reads the body.
	if notReady["code"] == paused["code"] {
		t.Fatalf("`not wired` and `operator-paused` returned the SAME code %v over 503. They demand opposite client behaviour (retry-elsewhere-and-page vs retry-later); collapsing them re-creates the pre-item-18 state where the two were distinguishable only by free text.", paused["code"])
	}
	if notReady["message"] == paused["message"] {
		t.Fatalf("both 503s carry the same message %v — the codes differ but a human reading the body still cannot tell which happened", paused["message"])
	}
	// ANTI-VACUITY: two 503s prove nothing if the fixture is simply broken in both cases. The SAME
	// request at `serving` on a wired server must not be 503 at all.
	srvServing, _, _ := deferServer(t, now, config.FrontDoorServing)
	if rec := do(t, srvServing, http.MethodPost, "/v1/verify", body); rec.Code == http.StatusServiceUnavailable {
		t.Fatalf("control failed: a wired, serving server ALSO answers 503 (%s) — the two codes above prove nothing", rec.Body.String())
	}
}

// TestErrorEnvelope_BulkCapNoLongerAnswersInASecondShape — audit classification 2.7.
//
// This route answered `{"error": "...", "max": N, "got": M}`: a bespoke envelope on exactly one of
// 26 sites, so every client had to special-case it. It now answers the standard envelope, and the
// two numbers reach the caller through details[] (which field was out of range) and the operator
// through the logged internalReason.
//
// FAILS IF: the `max`/`got` keys come back at the top level or inside the error object; the route
// reverts to a bespoke body; details[] loses the out_of_range signal.
func TestErrorEnvelope_BulkCapNoLongerAnswersInASecondShape(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	cfg := config.Defaults()
	cfg.Env = "production"
	sec, _ := secrets.Load(fakeSource{}, false)
	h := mustHasher(t)
	mem := store.NewMem(func() time.Time { return now })
	def, _ := policy.ColdEmailDefinition()
	// A cap of 1 so the refusal needs two addresses, not 100_001 — the cap VALUE is not what is
	// under test, the response SHAPE is.
	srv := New(cfg, sec, h).WithVerifier(stubVerifier{}, def).WithCallerAuth(mem).WithBulk(nopSubmitter{}, mem, 1)
	srv.now = func() time.Time { return now }
	mem.SeedCallerWithUser(sha("cap-key"), "caller_cap", bulkOwner, now.Add(-time.Hour), time.Time{})

	r := httptest.NewRequest(http.MethodPost, "/v1/verify/bulk",
		strings.NewReader(`{"addresses":["a@acmecorp.com","b@acmecorp.com"]}`))
	r.Header.Set("Authorization", "Bearer cap-key")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)

	body := requireEnvelope(t, rec, codePayloadTooLarge, http.StatusRequestEntityTooLarge)
	requireDetail(t, body, "addresses", issueOutOfRange)

	// The second shape's own fields must be gone from the response entirely — not merely absent from
	// the top level, since "moved inside the error object" would still be a bespoke shape.
	for _, k := range []string{"max", "got"} {
		if _, present := body[k]; present {
			t.Fatalf("error object still carries the second shape's %q key: %s", k, rec.Body.String())
		}
	}
	if strings.Contains(rec.Body.String(), `"max"`) || strings.Contains(rec.Body.String(), `"got"`) {
		t.Fatalf("the bulk-cap route still answers in the second shape (max/got): %s", rec.Body.String())
	}

	// ANTI-VACUITY: the same submission UNDER the cap must be accepted. Without this, a route that
	//413'd everything — or one broken so no bulk request ever succeeds — would satisfy the above.
	rOK := httptest.NewRequest(http.MethodPost, "/v1/verify/bulk", strings.NewReader(`{"addresses":["a@acmecorp.com"]}`))
	rOK.Header.Set("Authorization", "Bearer cap-key")
	recOK := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recOK, rOK)
	if recOK.Code != http.StatusAccepted {
		t.Fatalf("control failed: a submission of 1 address at cap=1 returned %d, want 202 — the refusal above proves nothing about the cap: %s",
			recOK.Code, recOK.Body.String())
	}
}

// nopSubmitter is a BulkSubmitter that accepts anything. The cap refusal happens BEFORE the
// submitter is reached, and the anti-vacuity control needs the under-cap path to reach a 202 without
// standing up a real Enqueuer.
type nopSubmitter struct{}

func (nopSubmitter) SubmitBulkRows(_ context.Context, rows []queue.BulkRow, _ *policy.Definition, _, _ string) (queue.BulkResult, error) {
	return queue.BulkResult{JobID: "019fd000-0000-7000-8000-0000000000aa", Total: len(rows), Enqueued: len(rows)}, nil
}

// TestErrorEnvelope_RequestIDsAreUniquePerRequest — a correlation id that repeats correlates
// nothing, and a constant is exactly what a well-meaning "avoid allocating per request" change would
// introduce.
//
// FAILS IF: newRequestID is replaced by a constant or a package-level cached value; request_id is
// dropped (empty ids would collide); the minter starts returning `req_<hex>` (bareUUID rejects it).
func TestErrorEnvelope_RequestIDsAreUniquePerRequest(t *testing.T) {
	srv := unwiredServer(t)
	seen := map[string]bool{}
	const n = 20
	for i := 0; i < n; i++ {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/verify",
			strings.NewReader(`{"email":"jane@acmecorp.com"}`)))
		body := requireEnvelope(t, rec, codeUnavailable, http.StatusServiceUnavailable)
		id, _ := body["request_id"].(string)
		if seen[id] {
			t.Fatalf("request_id %q repeated across %d identical requests — a correlation id that repeats cannot correlate a user report to a log line", id, n)
		}
		seen[id] = true
	}
	if len(seen) != n {
		t.Fatalf("saw %d distinct request_ids across %d requests", len(seen), n)
	}
}

// TestErrorEnvelope_CodeRegistryIsComplete — every declared code must have BOTH a status and a
// curated message, and every status must be a real error status.
//
// This is not busywork: writeErrWithID looks up codeStatus and falls back to internal_error on a
// miss, and reads fixedMessages with no fallback at all. A code added to the const block but not to
// both maps therefore serializes as `message: ""` or answers the wrong status — a defect that is
// invisible until that specific error fires in production.
//
// FAILS IF: a code is added to one map and not the other, or given a 2xx/3xx status.
func TestErrorEnvelope_CodeRegistryIsComplete(t *testing.T) {
	all := []string{
		codeUnauthenticated, codeValidationFailed, codeNotFound, codeMethodNotAllowed,
		codePayloadTooLarge, codeUnavailable, codePaused, codeInternalError, codeContractViolation,
	}
	// codeMethodNotAllowed was added by fix-plan item 14, and THIS TEST is what noticed. The code
	// went into the const block and both maps, `go test ./internal/api/` was run in isolation on the
	// new route tests and passed, and the full-suite run then failed here with "codeStatus carries
	// method_not_allowed, which is not in this test's declared list". That orphan branch is doing
	// exactly the job its header describes — a new code must be declared in two places and the second
	// one is a test, so adding a code cannot be a silent act. Worth recording because the same run
	// also shows why per-package isolation is not a substitute for the full suite.
	for _, c := range all {
		status, ok := codeStatus[c]
		if !ok {
			t.Errorf("code %q has no codeStatus entry — writeErr would answer 500 with internal_error instead", c)
			continue
		}
		if status < 400 || status > 599 {
			t.Errorf("code %q maps to status %d, which is not an error status", c, status)
		}
		if fixedMessages[c] == "" {
			t.Errorf("code %q has no fixedMessages entry — it would serialize as message:\"\"", c)
		}
	}
	// And the maps must not carry codes the const block doesn't declare (a rename leaving an orphan).
	declared := map[string]bool{}
	for _, c := range all {
		declared[c] = true
	}
	for c := range codeStatus {
		if !declared[c] {
			t.Errorf("codeStatus carries %q, which is not in this test's declared list — either a new code was added without extending this test, or a rename left an orphan", c)
		}
	}
	for c := range fixedMessages {
		if !declared[c] {
			t.Errorf("fixedMessages carries %q, which is not in this test's declared list", c)
		}
	}
}

// TestErrorEnvelope_CodesSharedWithTheSendersMatchByteForByte — the cross-module half of item 18.
//
// The point of a coded envelope is that a platform client's error handling is ONE switch across the
// three modules. That only holds if the shared codes are spelled identically, and "identically" is
// not something a comment can promise: a plausible-looking `"unauthorized"` here instead of
// `"unauthenticated"` would silently split the client's switch in two.
//
// M1 deliberately carries codes the senders have no equivalent condition for (contract_violation,
// payload_too_large, unavailable) and lacks several of theirs — this asserts the OVERLAP is
// exact, not that the sets are equal.
//
// `paused` MOVED INTO THE OVERLAP ON 2026-08-05. It was listed above as M1-only until fix-plan item
// 12 gave both senders a graduated dispatch rung ladder whose two deepest rungs answer `503 paused`
// on their intake route (their ADR-0017). The condition is the same on all three modules — an
// operator pulled a kill-switch, and this exact request will succeed later unchanged — so the code
// and its 503 are now genuinely cross-module and belong in this assertion. That it had to be MOVED,
// rather than having been caught by anything, is the honest lesson: a comment asserting a code was
// module-local kept saying so for as long as nobody re-read it.
//
// LIMIT, stated rather than assumed away: the senders' spellings are HARDCODED here, because the
// three modules are separate repos and separate Go modules by design (OD-7 isolation) — this test
// cannot read their source, so it pins M1's side of the contract against a transcribed copy of
// theirs. If a SENDER renames a shared code, nothing here notices. That direction is item 14's job
// (the cross-adopt response-version assertion); it is not silently covered by this test, and saying
// so is the point — an unverified claim of immunity is the defect that produced this whole item.
func TestErrorEnvelope_CodesSharedWithTheSendersMatchByteForByte(t *testing.T) {
	// Verified against send-byo/send-managed platformapi/errors.go, not assumed.
	senderSpellings := map[string]string{
		"unauthenticated":   codeUnauthenticated,
		"validation_failed": codeValidationFailed,
		"not_found":         codeNotFound,
		"internal_error":    codeInternalError,
		"paused":            codePaused, // added 2026-08-05 — see the header note
		// added 2026-08-06 (fix-plan item 14). Before that, M1 answered a wrong verb with ServeMux's
		// plain-text 405 — the code did not exist here at all, which is why the header's list of
		// "codes only the senders carry" named it. Transcribed from both senders'
		// services/control/internal/platformapi/errors.go, same stated limit as the rest.
		"method_not_allowed": codeMethodNotAllowed,
	}
	for want, got := range senderSpellings {
		if got != want {
			t.Errorf("M1 spells the shared code %q as %q — a client switching on error.code across the three modules would need two branches for one condition", want, got)
		}
	}

	// The STATUS is half the contract. A client meeting an unrecognised code degrades by status
	// (item 7's unknown-code rule), so two modules agreeing on the spelling of `paused` while
	// disagreeing on whether it is a 503 or a 409 would break exactly the clients this shared
	// vocabulary is for — and would do it silently, since the switch arm they share would still
	// match. The senders' values are transcribed, with the same stated limit as the spellings above.
	senderStatuses := map[string]int{
		"unauthenticated":    401,
		"validation_failed":  400,
		"not_found":          404,
		"internal_error":     500,
		"paused":             503,
		"method_not_allowed": 405,
	}
	for code, want := range senderStatuses {
		got, ok := codeStatus[code]
		if !ok {
			t.Errorf("M1's codeStatus has no entry for the shared code %q — it cannot be emitted here at all, so this module's half of the shared vocabulary is incomplete", code)
			continue
		}
		if got != want {
			t.Errorf("M1 maps the shared code %q to HTTP %d, the senders map it to %d — a client that degrades an unrecognised code by status (item 7) would take a different branch per module for one condition", code, got, want)
		}
	}
}

// TestErrorEnvelope_NoHandRolledErrorBodiesRemain — THE load-bearing test of item 18.
//
// Every other test here checks a specific route. This one reads api.go's SOURCE and fails if ANY
// error body is written outside writeErr/writeErrWithID. That distinction matters because the defect
// was 26 independent call sites: a reviewer looking at a diff sees the serializer arrive and reads
// "errors are centralized now," while a single remaining hand-rolled site — or a new one added next
// month — reproduces the whole problem. No route-by-route test can catch a site that does not exist
// yet, and the audit's own note is that each code must be "chosen deliberately rather than mapped by
// rote," which only stays true if adding an error site forces you through the serializer.
//
// It works by enumerating writeJSON's error-status call sites: writeJSON is the raw writer, and
// after item 18 every non-2xx use of it must be the one inside writeErrWithID.
//
// IT SCANS EVERY NON-TEST .go FILE IN THE PACKAGE, NOT JUST api.go. The first version of this test
// read api.go alone, which was a hole found by breaking it: moving a handler into a new file left
// the scan blind to it while everything stayed green. That is this item's own failure family — a
// check whose coverage is narrower than the thing it claims to guard — so the fix is a mechanism
// (glob the package) and not a comment telling the next person to remember api.go.
//
// FAILS IF: someone adds `writeJSON(w, http.StatusBadRequest, ...)` anywhere in the package
// (verified), reintroduces a top-level `"error":` map literal (verified), or adds a new handler file
// that hand-rolls errors (verified — the case that motivated the glob).
func TestErrorEnvelope_NoHandRolledErrorBodiesRemain(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var scanned []string
	// (a) No writeJSON with a non-2xx literal status. The 2xx ones are the success paths and are
	// correct; anything else is an error body bypassing the serializer.
	errStatus := regexp.MustCompile(`writeJSON\(w,\s*http\.Status(\w+)`)
	okStatuses := map[string]bool{"OK": true, "Accepted": true, "Created": true, "NoContent": true}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		scanned = append(scanned, name)
		for i, raw := range strings.Split(string(src), "\n") {
			// STRIP COMMENTS BEFORE SCANNING. errors.go's own header DESCRIBES the old shape
			// (`{"error":"<free text>"}`) in order to explain what item 18 removed, and a check that
			// cannot tell prose from code would flag that documentation as the defect — the
			// mirror-image of the failure family this whole pass is about, where prose was mistaken
			// for a mechanism. Only the code half of each line is scanned; anything before a `//` is
			// still checked, so a hand-rolled body cannot hide by parking a comment after it.
			// (Verified: no non-test file in this package has `//` inside a string literal, so a
			// naive cut is safe here — this is a package-local check, not a general Go parser.)
			ln := raw
			if idx := strings.Index(ln, "//"); idx >= 0 {
				ln = ln[:idx]
			}
			if m := errStatus.FindStringSubmatch(ln); m != nil && !okStatuses[m[1]] {
				t.Errorf("%s:%d writes an error body directly through writeJSON (http.Status%s), bypassing writeErr. Every error in this package must go through the ONE serializer, or it will lack a code, a curated message, and a request_id — the exact defect fix-plan item 18 closed:\n  %s",
					name, i+1, m[1], strings.TrimSpace(raw))
			}
			// (b) No hand-rolled `"error":` body literal. The old shape was
			// map[string]any{"error": "..."}; this catches it even when written through a helper
			// whose status this test cannot see. errors.go's own `json:"error"` struct tag is not a
			// map literal and does not match (the pattern requires the colon immediately after the
			// closing quote).
			if strings.Contains(ln, `"error":`) {
				t.Errorf("%s:%d contains a hand-rolled `\"error\":` body literal — the pre-item-18 shape. Use writeErr:\n  %s",
					name, i+1, strings.TrimSpace(raw))
			}
		}
	}

	// ANTI-VACUITY, and the reason this test is trustworthy at all: a scan proves nothing if it
	// scanned nothing. A wrong working directory, a renamed package layout, or a filter bug all
	// produce zero files and a green pass. So assert the scan really covered this package's source.
	if len(scanned) < 3 {
		t.Fatalf("anti-vacuity: only scanned %v — this package has api.go, errors.go, and helpers.go at minimum, so the check above covered almost nothing", scanned)
	}
	// And assert the SPECIFIC things the regexes need to have been able to match: the front door's
	// handlers (wherever they now live) and at least one success writeJSON, so a build where
	// writeJSON was renamed cannot pass by matching zero lines everywhere.
	joined := ""
	for _, name := range scanned {
		b, _ := os.ReadFile(name)
		joined += string(b)
	}
	for _, needle := range []string{"func (s *Server) handleVerify", "writeJSON(w, http.StatusOK", "func writeErr("} {
		if !strings.Contains(joined, needle) {
			t.Fatalf("anti-vacuity: no scanned file (%v) contains %q, so the scan above proved nothing — either the serializer was renamed or the handlers left this package", scanned, needle)
		}
	}
}

// TestErrorEnvelope_UnregisteredCodeFailsSafe — writeErrWithID's own defensive branch.
//
// An unregistered code is a programming error. The branch answers a REGISTERED internal_error rather
// than a bare status, because an envelope carrying an unknown code is worse for a client than a
// known 500: it breaks the switch the codes exist to enable. Tested directly because no route can
// reach it — which is exactly why it would otherwise rot untested.
//
// FAILS IF: the fallback is removed (a 200 with an unknown code, or an empty message), or it answers
// with the unregistered code passed through.
func TestErrorEnvelope_UnregisteredCodeFailsSafe(t *testing.T) {
	rec := httptest.NewRecorder()
	writeErr(rec, "no_such_code_was_ever_registered", "deliberate: exercising the defensive branch")

	body := requireEnvelope(t, rec, codeInternalError, http.StatusInternalServerError)
	if got, _ := body["code"].(string); got == "no_such_code_was_ever_registered" {
		t.Fatalf("an unregistered code was passed through to the caller: %s", rec.Body.String())
	}
}
