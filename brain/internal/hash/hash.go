// Package hash implements Door 3: the FROZEN email normalization + hashing scheme.
// (PROJECT_RULES.md Door 3; P1 §3.3.)
//
// Scheme tag: "norm/v1+hmac-sha256/v1". This is a ONE-WAY DOOR — the normalization
// rules and the HMAC construction are LOCKED and may never change in place; a change
// re-partitions every historical row and de-joins the P7 flywheel. Because the scheme
// is frozen and pure, it is implemented in full at Phase 0 (getting it right early is
// the whole point) — the only non-frozen input is the pepper, which is a SECRET loaded
// from secure config and NEVER hardcoded or committed (the system's most fragile artifact).
//
// What is stored (P1 §3.3.1): email_hash of the normalized FULL email (bytea, 32 bytes);
// domain in clear; the raw full email is NEVER persisted.
package hash

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"strings"
)

// SchemeTag is the frozen provenance tag stamped on every hashed row (P1 §3.3.6).
// It is provenance, NOT a license to change the scheme; v1 is frozen.
const SchemeTag = "norm/v1+hmac-sha256/v1"

// ErrUnparseable is returned when an address cannot be normalized (e.g. not exactly
// one '@'). Per P1 §3.2.7 such rows are LOGGED with a NULL email_hash — they are
// "un-addressable rejects", never an error to swallow and never hashed.
var ErrUnparseable = errors.New("hash: address is unparseable (norm/v1); log with NULL email_hash")

// Hasher holds the global HMAC pepper. Construct it from a secret loaded via the
// config/secrets scaffold — NEVER from a literal. Losing the pepper de-joins all
// history; leaking it makes low-entropy local-parts brute-forceable (P1 §3.3.3).
type Hasher struct {
	pepper []byte
}

// NewHasher wires the pepper. A zero-length pepper is refused: an unpeppered hash
// is a plain SHA-256, which is reversible for the enumerable email space — exactly
// the breach Door 3 exists to prevent.
func NewHasher(pepper []byte) (*Hasher, error) {
	if len(pepper) == 0 {
		return nil, errors.New("hash: refusing an empty pepper (Door 3 — would make hashes reversible)")
	}
	return &Hasher{pepper: pepper}, nil
}

// Normalize applies the LOCKED norm/v1 pipeline (P1 §3.3.2), deterministic + ordered:
//  1. trim whitespace
//  2. require exactly one '@' (split on the LAST '@')
//  3. domain: lowercase, strip one trailing dot, IDNA ToASCII (punycode), NFC
//  4. local: NFC, lowercase (OQ-3, RFC-lossy accepted); do NOT strip dots or fold +tags
//  5. recompose local@domain
//
// Returns the normalized email and the normalized domain. On failure at step 2 it
// returns ErrUnparseable (caller logs with NULL hash + NULL domain — P1 §3.2.7).
//
// NOTE (Phase 0 honesty — stdlib-only, no network deps): the full norm/v1 pipeline
// needs Unicode NFC (x/text/unicode/norm) and IDNA ToASCII/punycode (x/net/idna).
// Phase 0 is deliberately stdlib-only so the skeleton builds with zero third-party
// fetches. So this implements the ASCII path in full and marks the Unicode/IDNA steps
// as the explicit P1 wiring point — it does NOT silently pretend to punycode. An input
// with non-ASCII in the domain is returned ErrUnparseable here rather than mis-hashed,
// so no wrong (under-normalized) hash can enter history before the IDNA step is wired.
func (h *Hasher) Normalize(raw string) (email, domain string, err error) {
	s := strings.TrimSpace(raw)
	at := strings.LastIndex(s, "@")
	if at <= 0 || at == len(s)-1 || strings.Count(s, "@") != 1 {
		return "", "", ErrUnparseable
	}
	local, dom := s[:at], s[at+1:]

	// Domain side (ASCII path).
	dom = strings.ToLower(dom)
	dom = strings.TrimSuffix(dom, ".")
	// P1 WIRING POINT: replace this ASCII guard with idna.Lookup.ToASCII(dom) + NFC.
	// Refusing non-ASCII here (rather than hashing it un-punycoded) prevents a wrong
	// hash from entering the frozen store before the IDNA step lands.
	if !isASCII(dom) {
		return "", "", ErrUnparseable
	}

	// Local side: lowercase (OQ-3); keep dots and +tags (provider-specific, kept separate).
	// P1 WIRING POINT: Unicode NFC on the local part before lowercasing.
	// Same refusal as the domain: a non-ASCII local part must NOT be hashed without NFC,
	// or the composed vs decomposed byte forms of one address produce two different hashes
	// (splitting one identity in the frozen store). Refuse until NFC is wired at P1.
	if !isASCII(local) {
		return "", "", ErrUnparseable
	}
	local = strings.ToLower(local)

	return local + "@" + dom, dom, nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// EmailHash computes email_hash = HMAC_SHA256(pepper, normalized_email), returned as
// the raw 32-byte value (stored as bytea — NOT hex; P1 §3.3.3). Deterministic, so it
// is a stable dedup/cache/join key. Caller must pass the ALREADY-normalized email.
func (h *Hasher) EmailHash(normalizedEmail string) []byte {
	mac := hmac.New(sha256.New, h.pepper)
	mac.Write([]byte(normalizedEmail))
	return mac.Sum(nil)
}

// HashRaw is the convenience path: normalize then hash. Returns ErrUnparseable for
// un-addressable input (caller logs NULL hash per P1 §3.2.7).
func (h *Hasher) HashRaw(raw string) (emailHash []byte, domain string, err error) {
	norm, dom, err := h.Normalize(raw)
	if err != nil {
		return nil, "", err
	}
	return h.EmailHash(norm), dom, nil
}
