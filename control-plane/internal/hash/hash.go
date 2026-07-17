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
//
// Phase 1 (ADR-0006): the NFC + IDNA steps that Phase 0 deferred are now WIRED. The
// composed vs. decomposed byte forms of one address NFC-normalize to the SAME bytes, so
// they hash identically (the split-identity bug ADR-0006 blessed the interim refusal of).
// Per ADR-0006 both the local- and domain-side non-ASCII guards are relaxed TOGETHER —
// an asymmetric relax would reintroduce the split. A domain that IDNA-ToASCII cannot encode
// is still ErrUnparseable (logged NULL — P1 §3.2.7), never mis-hashed.
package hash

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"strings"

	"golang.org/x/net/idna"
	"golang.org/x/text/unicode/norm"
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
//  3. domain: strip one trailing dot, IDNA/UTS-46 ToASCII (punycode, lowercases), then NFC
//  4. local: NFC, lowercase (OQ-3, RFC-lossy accepted); do NOT strip dots or fold +tags
//  5. recompose local@domain
//
// Returns the normalized email and the normalized domain. On failure at step 2 (bad
// '@' structure) or step 3 (a domain IDNA cannot encode) it returns ErrUnparseable
// (caller logs with NULL hash + NULL domain — P1 §3.2.7).
//
// Phase 1 (ADR-0006): NFC + IDNA are now wired. NFC is applied to BOTH sides so the
// composed and decomposed byte forms of the same address collapse to identical bytes
// (and thus one hash). Both non-ASCII guards were relaxed together, per ADR-0006 — an
// asymmetric relax would reintroduce the split-identity bug. The only remaining refusal
// on the domain is a genuine IDNA encoding failure, which is un-addressable, not
// under-normalized — so no wrong hash can enter the frozen store.
func (h *Hasher) Normalize(raw string) (email, domain string, err error) {
	s := strings.TrimSpace(raw)
	at := strings.LastIndex(s, "@")
	if at <= 0 || at == len(s)-1 || strings.Count(s, "@") != 1 {
		return "", "", ErrUnparseable
	}
	local, dom := s[:at], s[at+1:]

	// Domain side: IDNA/UTS-46 ToASCII (punycode) FIRST, then strip trailing dot(s).
	// The strip MUST come after ToASCII: UTS-46 maps the Unicode label separators
	// U+3002/U+FF0E/U+FF61 (。．｡) to ASCII '.', so a trailing CJK separator only becomes a
	// trailing dot during ToASCII — stripping before would miss it, splitting "example.com。"
	// from "example.com". idnaProfile lowercases + validates; a domain it cannot encode
	// (illegal label, disallowed rune) is un-addressable → ErrUnparseable, never mis-hashed.
	dom, err = idnaProfile.ToASCII(dom)
	if err != nil {
		return "", "", ErrUnparseable
	}
	dom = strings.TrimRight(dom, ".") // FQDN + CJK-separator + double-dot all collapse here.
	if dom == "" {
		return "", "", ErrUnparseable
	}

	// Local side: NFC → lowercase → NFC AGAIN. The trailing NFC is load-bearing, not
	// belt-and-suspenders: Unicode case mapping is NOT closed under NFC (UAX#15), so
	// ToLower can emit a non-NFC sequence — without the second NFC, an address and its own
	// case-variant twin (e.g. "J"+combining-caron vs precomposed "ǰ") hash DIFFERENTLY,
	// splitting one identity (the same corruption class as ADR-0006, via case+mark). Verified:
	// the trailing NFC is a strict no-op on all ASCII (no already-valid ASCII hash shifts) and
	// closes every base-letter+combining-mark case split EXCEPT the Turkish dotted/dotless-I
	// class (U+0130 / I+U+0307), a documented frozen edge — see TestLocalCaseFoldFrozenEdge.
	// Keep dots and +tags (provider quirks live only in ProviderCanonicalHash, never here).
	local = norm.NFC.String(strings.ToLower(norm.NFC.String(local)))

	return local + "@" + dom, dom, nil
}

// idnaProfile is the locked IDNA/UTS-46 profile for norm/v1 domain canonicalization.
// MapForLookup (case-fold + map) + Transitional(false) + StrictDomainName(true):
// deterministic ToASCII that encodes real IDNs (exämple.com → xn--…, münchen.de → xn--…)
// yet REJECTS genuine garbage — disallowed runes (NUL, '_') and invalid labels ('-') —
// so un-addressable input becomes ErrUnparseable (NULL hash) rather than a bogus key.
// Not idna.Lookup, whose extra std-resolution validity checks can refuse some registered
// IDNs we must still hash consistently. Frozen: changing this re-partitions history.
var idnaProfile = idna.New(
	idna.MapForLookup(),
	idna.Transitional(false),
	idna.StrictDomainName(true),
)

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
	n, dom, err := h.Normalize(raw)
	if err != nil {
		return nil, "", err
	}
	return h.EmailHash(n), dom, nil
}

// ProviderCanonicalHash computes the OPTIONAL provider-aware canonical hash (P1 §3.3.5,
// OQ-2): an ADDITIONAL alias-dedup/rollup key, NEVER the primary identity. Provider quirks
// (gmail ignores '.' and everything after '+') are folded ONLY here, so distinct addresses
// on providers that treat dots/tags as significant never collide in the primary email_hash.
//
// It MUST be populated from row #1: it needs the normalized address, which is never
// persisted, so it cannot be back-computed later (OQ-2 rationale). Returns ok=false when
// the provider is unknown/empty (caller stores NULL) — never a guessed canonicalization.
//
// The input is the ALREADY-normalized email (norm/v1 output). Aliasing is gated on the
// actual gmail domains, NOT the whole "google" class: Google Workspace custom domains do
// not universally ignore dots, so folding them there would wrongly collide distinct
// mailboxes. For every other known provider the canonical form == the normalized form.
func (h *Hasher) ProviderCanonicalHash(normalizedEmail, provider string) (hash []byte, ok bool) {
	canon, ok := providerCanonicalize(normalizedEmail, provider)
	if !ok {
		return nil, false
	}
	return h.EmailHash(canon), true
}

func providerCanonicalize(normalizedEmail, provider string) (string, bool) {
	if provider == "" { // provider not classified → NULL, never guessed.
		return "", false
	}
	at := strings.LastIndex(normalizedEmail, "@")
	if at <= 0 || at == len(normalizedEmail)-1 {
		return "", false
	}
	local, dom := normalizedEmail[:at], normalizedEmail[at+1:]
	// Gmail alias semantics apply only to the real gmail domains (not all "google" —
	// Workspace domains may treat dots as significant, §3.3.5 "trap avoided").
	if dom == "gmail.com" || dom == "googlemail.com" {
		if i := strings.IndexByte(local, '+'); i >= 0 {
			local = local[:i]
		}
		local = strings.ReplaceAll(local, ".", "")
		// A folded-away local (e.g. "+tag@" or ".@") would collapse distinct inputs onto a
		// degenerate "@gmail.com" key. Such an address is undeliverable anyway → NULL, not a
		// bogus shared canonical.
		if local == "" {
			return "", false
		}
	}
	return local + "@" + dom, true
}
