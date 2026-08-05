package api

// errors.go — fix-plan item 18 / audit D3, classifications 2.1-2.4, 2.7, UI questions 5-7,
// canonical-module-contract.md §1.3. The coded error envelope, and the ONE serializer that writes it.
//
// WHAT WAS WRONG. M1 hand-rolled 26 error sites as `{"error":"<free text>"}` — no machine-readable
// code, no request_id in ANY error body, and one route answered in a SECOND shape entirely
// (`{error, max, got}` on the bulk size refusal). The success path already returns
// `meta.request_id` (verdict.Response.RequestID, minted by orchestrator.mustID); the error path
// simply dropped it. So a user-reported M1 failure was correlatable to NOTHING: no code to branch
// on, no id in the body, and no id in any log line either. The audit called this "the most
// operationally expensive small gap," and worth doing even if the M1 platform surface (Tier 4) is
// declined, because it improves M1's existing bespoke surface on its own terms.
//
// THE MINTER IS idgen, NOT A NEW ONE. This package previously had a `newRequestID` returning
// `req_<hex>` — dead code with zero call sites, and WRONG: the LOG's request_id column is Postgres
// `uuid` (§3.2.2), so `req_...` could never be persisted. It was deleted under audit D23 with a note
// naming this work as the thing that must not resurrect it. M3's platformapi mints `req_<hex>` and
// that is correct FOR M3 (its correlation id is opaque and never stored as a uuid) — copying it here
// would reintroduce exactly the defect D23 removed. M1 uses idgen.NewV7String: one minter, one
// format, valid in the uuid column, sortable by time.
//
// WHY A CODE PER SITE, CHOSEN AND NOT MAPPED BY ROTE. The audit's cost note says each of the 26
// needs a code "chosen deliberately rather than mapped by rote," and two cases prove why:
//
//   - `not ready` and `service paused` BOTH answered 503 before. They are different failures — one
//     is "this deployment is not wired" (missing DB/pepper), the other is "an operator pulled
//     kill-switch Rung 4 deliberately." A client retrying blindly is right in one case and wrong in
//     the other, and an operator seeing `unavailable` in a triage log cannot tell which happened.
//     They get DISTINCT codes over the same 503.
//   - `internal contract violation` (api.go:232) is not an ordinary 500: it fires when the
//     orchestrator returns a verdict that fails the Door-1 shape check. It keeps its own code so a
//     door violation is never lost inside generic internal_error noise.
//
// TWO-VOCABULARY RULE. `message` is a curated constant sentence, chosen from fixedMessages by code
// and never interpolated from request input; `internalReason` is for the log line and is NEVER
// serialized. Nothing about the caller's input is echoed back — the bulk size refusal's old
// `{max, got}` fields are the one place M1 did echo, and they move into `details[]` as a fixed
// issue slug, so the numbers still reach the caller without a free-text channel.

import (
	"log"
	"net/http"
	"time"

	"github.com/coldmail-verify/control-plane/internal/idgen"
)

// The code enum. ADDITIVE-ONLY: never rename, never repurpose (Door 4 / §6's two-vocabulary rule).
//
// WHERE THE SLUGS COME FROM. The four M1 shares with the senders — unauthenticated,
// validation_failed, not_found, internal_error — are spelled BYTE-IDENTICALLY to
// send-byo/send-managed platformapi/errors.go, so a platform client's error handling across the
// three modules is one switch and not three. Checked against their source, not assumed, and held by
// TestErrorEnvelope_CodesSharedWithTheSendersMatchByteForByte.
//
// The other four are M1-ONLY, and this is NOT "a subset plus two": payload_too_large, unavailable,
// paused, and contract_violation all describe conditions the senders have no equivalent of (a
// submission cap, an unwired deployment, kill-switch Rung 4, a Door-1 shape check). Equally, M1 does
// NOT carry method_not_allowed / conflict / unprocessable / rate_limited, which the senders do. The
// overlap is exact where it exists; the sets are deliberately different sizes, because the modules
// genuinely fail in different ways and a shared code that meant something slightly different per
// module would be worse than no shared code at all.
const (
	codeUnauthenticated   = "unauthenticated"    // 401
	codeValidationFailed  = "validation_failed"  // 400
	codeNotFound          = "not_found"          // 404
	codePayloadTooLarge   = "payload_too_large"  // 413
	codeUnavailable       = "unavailable"        // 503 — this deployment is not wired (no DB/pepper)
	codePaused            = "paused"             // 503 — an operator pulled kill-switch Rung 4
	codeInternalError     = "internal_error"     // 500
	codeContractViolation = "contract_violation" // 500 — a Door-1 shape check failed
)

// The field-level issue vocabulary. Fixed slugs, never free text (§1.3's details[].issue).
const (
	issueRequired   = "required"
	issueMalformed  = "malformed"
	issueOutOfRange = "out_of_range"
	issueNotFound   = "not_found"
)

// fixedMessages — the ONLY sentences that reach a caller. One per code, curated, no interpolation.
var fixedMessages = map[string]string{
	codeUnauthenticated:   "Missing or invalid credentials.",
	codeValidationFailed:  "One or more fields are invalid.",
	codeNotFound:          "The requested resource was not found.",
	codePayloadTooLarge:   "The request is larger than this endpoint accepts.",
	codeUnavailable:       "The service is not ready to serve this request.",
	codePaused:            "The service is paused by an operator.",
	codeInternalError:     "An unexpected error occurred.",
	codeContractViolation: "An unexpected error occurred.",
}

// codeStatus — the HTTP status each code answers with. TWO codes share 503 and TWO share 500, on
// purpose: the status is what HTTP semantics require, the code is what the client branches on.
var codeStatus = map[string]int{
	codeUnauthenticated:   http.StatusUnauthorized,
	codeValidationFailed:  http.StatusBadRequest,
	codeNotFound:          http.StatusNotFound,
	codePayloadTooLarge:   http.StatusRequestEntityTooLarge,
	codeUnavailable:       http.StatusServiceUnavailable,
	codePaused:            http.StatusServiceUnavailable,
	codeInternalError:     http.StatusInternalServerError,
	codeContractViolation: http.StatusInternalServerError,
}

// fieldDetail is one structured field-level error. Field names are OUR identifiers (never echoed
// input), Issue is from the fixed slug set above.
type fieldDetail struct {
	Field string `json:"field"`
	Issue string `json:"issue"`
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code      string        `json:"code"`
	Message   string        `json:"message"`
	Details   []fieldDetail `json:"details,omitempty"`
	RequestID string        `json:"request_id"`
}

// newRequestID mints the correlation id for an error that has no orchestrator-minted one to reuse.
// A bare UUIDv7 (idgen), NOT the `req_<hex>` shape audit D23 deleted from this package: the LOG's
// request_id column is Postgres `uuid` (§3.2.2), so a prefixed id could never be persisted, and an
// id shape that cannot reach the log is worthless for the correlation this whole item exists for.
//
// On the vanishingly-unlikely crypto/rand failure it returns the all-zero-ish UUID rather than an
// empty string: `request_id: ""` in an error body reads as "this build does not emit ids," which is
// the exact confusion item 18 removes, whereas an obviously-degenerate UUID is still a valid uuid
// and is visibly a fallback. Same choice, same reasoning, as orchestrator.mustID.
func newRequestID() string {
	id, err := idgen.NewV7String(time.Now().UTC())
	if err != nil {
		return "00000000-0000-7000-8000-000000000000"
	}
	return id
}

// writeErr is the ONE error serializer. Every non-2xx body in this package goes through it.
//
// internalReason is LOGGED against the request id and never serialized — that pairing is the whole
// mechanism: an operator given a request_id by a user can grep one log line and see the real cause,
// while the caller receives a code, a curated sentence, and the same id. Before this, neither half
// existed.
func writeErr(w http.ResponseWriter, code, internalReason string, details ...fieldDetail) {
	writeErrWithID(w, newRequestID(), code, internalReason, details...)
}

// writeErrWithID is writeErr for a request that ALREADY has an id — a verdict that failed its
// Door-1 shape check carries the orchestrator's request_id, and reusing it means the error body, the
// LOG row, and the log line all name the SAME id. Minting a fresh one there would produce an id
// that appears in exactly one place, which is the state item 18 is fixing.
func writeErrWithID(w http.ResponseWriter, requestID, code, internalReason string, details ...fieldDetail) {
	status, ok := codeStatus[code]
	if !ok {
		// An unregistered code is a programming error, not a caller error. Answer 500 with a
		// registered code rather than a bare status: an envelope with an unknown code is worse for
		// a client than a known internal_error, and the log line names the offender.
		log.Printf("api: BUG — writeErr called with unregistered code %q (reason=%q request_id=%s); answering internal_error",
			code, internalReason, requestID)
		code, status = codeInternalError, http.StatusInternalServerError
	}
	if requestID == "" {
		requestID = newRequestID()
	}
	log.Printf("api error: code=%s status=%d request_id=%s reason=%q", code, status, requestID, internalReason)
	writeJSON(w, status, errorEnvelope{Error: errorBody{
		Code:      code,
		Message:   fixedMessages[code],
		Details:   details,
		RequestID: requestID,
	}})
}
