package api

import (
	"strings"
)

// DELETED: newRequestID (audit D23). It was dead code — zero call sites repo-wide — and it was also
// WRONG for the job its name claims, which is why deleting it beats wiring it up: it returned
// `req-<hex>`, and the LOG's `request_id` column is Postgres `uuid` (§3.2.2), so that value could
// never be persisted. The real minter is orchestrator.mustID (UUIDv7, bare, no prefix). Anything
// that needs a request id — including the coded-error-envelope work (fix-plan item 18) — must use
// that one; a second minter here would have been an invitation to produce ids the DB rejects.

// bearerToken extracts the token from `Authorization: Bearer <t>` on the FRONT DOOR, and returns ""
// for anything else. The scheme is MANDATORY: a bare token is rejected (canonical-module-contract §1
// clause 1, "a bare token MUST be rejected", which names M1's old laxness by name; the ruling itself
// is P1 OQ-13, resolved "Bearer for /v1/verify" at history/phase-1-foundations-spec.md:774).
//
// WHY THE OLD trimBearer's JUSTIFICATION WAS STALE, which is why the laxness survived a full audit.
// Its comment read "the front door is bearer; the verifier-node token is raw — kept distinct per P1
// §3.5.4", and both halves of that are true — but they are two DIFFERENT directions, so the second
// never licensed relaxing the first. The raw node token is sent OUTBOUND by internal/node/*_client.go
// (`req.Header.Set("Authorization", token) // raw node token (NOT Bearer)`, http_client.go:67) and is
// validated by the RUBY side (verifier-node .../wrapper/app.rb:176, the stock AccessToken service).
// It never reaches this package. trimBearer had exactly ONE caller — the inbound front door at
// api.go:692 — so the "kept distinct" clause was doing no work here at all, while its `return h`
// fallback silently accepted a scheme-less credential on every /v1 route.
//
// THE SCHEME MATCH IS CASE-INSENSITIVE, matching both senders (coldmail-send-byo
// .../platformapi/auth.go:56 and .../adminapi/auth.go, `strings.EqualFold(h[:len(prefix)], prefix)`).
// RFC 9110 §11.1 makes the auth scheme case-insensitive, so `bearer <t>` is a CONFORMING request that
// M1 alone used to reject — closing the laxness and fixing that strictness are the same edit, and a
// case-SENSITIVE fix here would have swapped one cross-module divergence for another.
func bearerToken(h string) string {
	const prefix = "bearer "
	h = strings.TrimSpace(h)
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// frontDoorToken returns the configured caller->control-plane bearer, if any.
func (s *Server) frontDoorToken() (string, bool) {
	if s.secrets == nil {
		return "", false
	}
	return s.secrets.FrontDoorToken()
}
