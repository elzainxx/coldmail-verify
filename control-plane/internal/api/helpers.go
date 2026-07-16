package api

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"
)

// newRequestID generates a server-side request id (P1 OQ-21: server-generated, client
// override allowed later). Uses crypto/rand; falls back to a time-derived id if rand fails.
func newRequestID(now time.Time) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req-" + now.UTC().Format("20060102T150405.000000000")
	}
	return "req-" + hex.EncodeToString(b[:])
}

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
