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

// trimBearer accepts both "Bearer <t>" and a raw token (the front door is bearer; the
// verifier-node token is raw — kept distinct per P1 §3.5.4).
func trimBearer(h string) string {
	h = strings.TrimSpace(h)
	if after, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(after)
	}
	return h
}

// frontDoorToken returns the configured caller->control-plane bearer, if any.
func (s *Server) frontDoorToken() (string, bool) {
	if s.secrets == nil {
		return "", false
	}
	return s.secrets.FrontDoorToken()
}
