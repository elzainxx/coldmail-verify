package hash

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// Door 3 is frozen. These tests pin the observable behavior of norm/v1 + hmac-sha256/v1
// so a change to the scheme (which would de-join all history) fails loudly.
//
// Non-ASCII test inputs are built with explicit \u escapes so the SOURCE stays pure ASCII
// (no editor/tool byte-encoding fragility): ö = composed 'ö'; 'o'+̈ = the
// decomposed form (o + combining diaeresis). NFC must collapse the two to one.

func newTestHasher(t *testing.T) *Hasher {
	t.Helper()
	h, err := NewHasher([]byte("test-pepper-not-a-real-one")) // real pepper is a loaded secret
	if err != nil {
		t.Fatalf("NewHasher: %v", err)
	}
	return h
}

func TestEmptyPepperRefused(t *testing.T) {
	if _, err := NewHasher(nil); err == nil {
		t.Fatal("an empty pepper must be refused (an unpeppered hash is reversible)")
	}
}

func TestNormalizeASCIIPath(t *testing.T) {
	h := newTestHasher(t)
	cases := []struct{ in, wantEmail, wantDomain string }{
		{"Jane.Doe@Example.COM", "jane.doe@example.com", "example.com"},      // lowercase both sides
		{"  user+tag@Example.com. ", "user+tag@example.com", "example.com"}, // trim, trailing dot, KEEP +tag & dots
		{"a@b.co", "a@b.co", "b.co"},
	}
	for _, c := range cases {
		email, domain, err := h.Normalize(c.in)
		if err != nil {
			t.Errorf("Normalize(%q) error: %v", c.in, err)
			continue
		}
		if email != c.wantEmail || domain != c.wantDomain {
			t.Errorf("Normalize(%q) = (%q,%q) want (%q,%q)", c.in, email, domain, c.wantEmail, c.wantDomain)
		}
	}
}

func TestNormalizeUnparseable(t *testing.T) {
	h := newTestHasher(t)
	for _, in := range []string{"noatsign", "two@@ats.com", "@nolocal.com", "trailingat@"} {
		if _, _, err := h.Normalize(in); err != ErrUnparseable {
			t.Errorf("Normalize(%q) err=%v, want ErrUnparseable (logged with NULL hash — P1 §3.2.7)", in, err)
		}
	}
}

// Phase 1 (ADR-0006): a non-ASCII domain is now IDNA-ToASCII'd (punycode), not refused.
// The Unicode form and its punycode form MUST normalize to the SAME domain (hence the
// same hash) — the whole point of IDNA in a frozen identity key.
func TestNonASCIIDomainPunycoded(t *testing.T) {
	h := newTestHasher(t)
	unicodeForm := "user@exämple.com"  // exämple.com
	punyForm := "user@xn--exmple-cua.com"    // its IDNA/punycode encoding
	eu, du, err := h.Normalize(unicodeForm)
	if err != nil {
		t.Fatalf("non-ASCII domain must now be punycoded (P1/ADR-0006), got err=%v", err)
	}
	if du != "xn--exmple-cua.com" {
		t.Errorf("domain must be IDNA-ToASCII: got %q want %q", du, "xn--exmple-cua.com")
	}
	ep, _, err := h.Normalize(punyForm)
	if err != nil {
		t.Fatalf("punycode domain: %v", err)
	}
	if !bytes.Equal(h.EmailHash(eu), h.EmailHash(ep)) {
		t.Errorf("unicode-domain and its punycode form must hash identically (%q vs %q)", eu, ep)
	}
}

// A domain IDNA genuinely cannot encode is un-addressable → ErrUnparseable (NULL hash,
// P1 §3.2.7), never mis-hashed. This is the ONLY remaining domain refusal.
func TestUnencodableDomainRejected(t *testing.T) {
	h := newTestHasher(t)
	for _, in := range []string{"user@" + string(rune(0)) + ".com", "user@-.com"} {
		if _, _, err := h.Normalize(in); err != ErrUnparseable {
			t.Errorf("IDNA-unencodable domain must be ErrUnparseable, got err=%v for %q", err, in)
		}
	}
}

// The regression ADR-0006 blessed the interim refusal of, now PROVEN FIXED: composed vs
// decomposed byte forms of the same visual local-part NFC-normalize to identical bytes,
// so they hash the SAME. Getting this wrong splits one identity in the frozen store. The
// test flipped from "must refuse" (P0) to "must hash equal" (P1) — the same invariant at
// its stronger, correct end-state (ADR-0006 "Compliance").
func TestNonASCIILocalPartNFCEqual(t *testing.T) {
	h := newTestHasher(t)
	composed := "jöhn@example.com"      // j ö hn      (ö = one code point, U+00F6)
	decomposed := "jöhn@example.com"   // j o ◌̈ hn   (o + U+0308 combining diaeresis)
	if composed == decomposed {
		t.Fatal("test setup: forms are byte-identical; the diacritic escapes did not differ")
	}
	ec, _, err := h.Normalize(composed)
	if err != nil {
		t.Fatalf("composed form must normalize (P1/ADR-0006), got %v", err)
	}
	ed, _, err := h.Normalize(decomposed)
	if err != nil {
		t.Fatalf("decomposed form must normalize (P1/ADR-0006), got %v", err)
	}
	if ec != ed {
		t.Fatalf("NFC must collapse composed/decomposed to identical bytes: %q vs %q", ec, ed)
	}
	if !bytes.Equal(h.EmailHash(ec), h.EmailHash(ed)) {
		t.Error("composed and decomposed local parts must hash identically after NFC")
	}
}

// ProviderCanonicalHash (OQ-2): gmail alias quirks fold ONLY in the canonical hash, never
// the primary. Distinct-looking gmail aliases share a canonical hash; the primary differs.
func TestProviderCanonicalHashGmailAliases(t *testing.T) {
	h := newTestHasher(t)
	forms := []string{"jane.doe@gmail.com", "janedoe@gmail.com", "jane.doe+news@gmail.com"}
	var canon, primary [][]byte
	for _, f := range forms {
		n, _, err := h.Normalize(f)
		if err != nil {
			t.Fatalf("normalize %q: %v", f, err)
		}
		c, ok := h.ProviderCanonicalHash(n, "google")
		if !ok {
			t.Fatalf("gmail provider canonical must be computable for %q", f)
		}
		canon = append(canon, c)
		primary = append(primary, h.EmailHash(n))
	}
	for i := 1; i < len(canon); i++ {
		if !bytes.Equal(canon[0], canon[i]) {
			t.Errorf("gmail aliases must share a provider_canonical_hash: %q vs %q", forms[0], forms[i])
		}
	}
	if bytes.Equal(primary[0], primary[1]) {
		t.Error("primary email_hash must NOT fold gmail dots — that is the trap §3.3.5 avoids")
	}
}

// Provider unknown → no canonicalization guessed (NULL, not a fabricated key).
func TestProviderCanonicalHashUnknownProvider(t *testing.T) {
	h := newTestHasher(t)
	if _, ok := h.ProviderCanonicalHash("a@b.co", ""); ok {
		t.Error("unknown provider must yield ok=false (store NULL), never a guessed canonical hash")
	}
}

// Non-gmail providers: canonical == normalized (no dot/tag folding off gmail).
func TestProviderCanonicalHashNonGmailIdentity(t *testing.T) {
	h := newTestHasher(t)
	n, _, _ := h.Normalize("jane.doe@corp.com")
	c, ok := h.ProviderCanonicalHash(n, "other")
	if !ok || !bytes.Equal(c, h.EmailHash(n)) {
		t.Error("non-gmail canonical hash must equal the primary (no alias folding off gmail)")
	}
}

func TestEmailHashDeterministicAndLength(t *testing.T) {
	h := newTestHasher(t)
	a := h.EmailHash("jane.doe@example.com")
	b := h.EmailHash("jane.doe@example.com")
	if !bytes.Equal(a, b) {
		t.Fatal("EmailHash must be deterministic (it is a dedup/cache/join key)")
	}
	if len(a) != 32 {
		t.Fatalf("email_hash must be 32 bytes (bytea, not hex text), got %d", len(a))
	}
	if bytes.Equal(a, h.EmailHash("john.doe@example.com")) {
		t.Fatal("distinct addresses must not collide")
	}
}

func TestPepperChangesHash(t *testing.T) {
	// The pepper is load-bearing: a different pepper => different hash (why losing it de-joins).
	h1, _ := NewHasher([]byte("pepper-one"))
	h2, _ := NewHasher([]byte("pepper-two"))
	if bytes.Equal(h1.EmailHash("a@b.co"), h2.EmailHash("a@b.co")) {
		t.Fatal("hash must depend on the pepper")
	}
}

func TestHashRawRejectsUnparseable(t *testing.T) {
	h := newTestHasher(t)
	if _, _, err := h.HashRaw("garbage"); err != ErrUnparseable {
		t.Errorf("HashRaw on unparseable must return ErrUnparseable, got %v", err)
	}
	// sanity: a good address produces a hex-representable 32-byte hash
	hh, _, err := h.HashRaw("a@b.co")
	if err != nil || len(hex.EncodeToString(hh)) != 64 {
		t.Errorf("HashRaw good address: err=%v len=%d", err, len(hh))
	}
}

// GOLDEN VECTOR — the single most important regression guard for a frozen HMAC door.
// It pins the ABSOLUTE output bytes of norm/v1+hmac-sha256/v1, so a silent change to the
// HMAC construction (tag-prefixing, keying order, hex-then-hash, double-HMAC) that keeps
// every relative-property test green would still FAIL here and de-join all history. The
// value was computed once from this exact construction; it must never change in place.
func TestGoldenVectorFrozen(t *testing.T) {
	if SchemeTag != "norm/v1+hmac-sha256/v1" {
		t.Fatalf("FROZEN scheme tag changed: %q", SchemeTag)
	}
	h, err := NewHasher([]byte("golden-pepper-v1"))
	if err != nil {
		t.Fatalf("NewHasher: %v", err)
	}
	const want = "8eef019d798d59cc6c7087f7c93634317b9e7600b0c8b1c05a36ebe6d84eefe0"
	got := hex.EncodeToString(h.EmailHash("jane.doe@example.com"))
	if got != want {
		t.Fatalf("FROZEN HMAC construction changed!\n got=%s\nwant=%s\n(any diff = every historical email_hash de-joins — Door 3)", got, want)
	}
}

// The Turkish dotted-I is a documented frozen edge (ADR-0006 / hash.go local-side note).
// What MUST hold: the composed İ (U+0130) and its decomposed form (I + U+0307) agree —
// NFC closure holds even here. What is DOCUMENTED: İ-derived forms lowercase to plain "i"
// (Go's simple case mapping drops the dot), a stable deterministic choice, distinct from a
// naturally-lowercase "i"+combining-dot. Frozen either way; pinned so it can't drift silently.
func TestLocalCaseFoldFrozenEdge(t *testing.T) {
	h := newTestHasher(t)
	composed := "İx@ex.com"    // İ (U+0130) + x
	decomposed := "İx@ex.com" // I + combining dot above + x
	ec, _, err1 := h.Normalize(composed)
	ed, _, err2 := h.Normalize(decomposed)
	if err1 != nil || err2 != nil {
		t.Fatalf("İ forms must normalize: %v / %v", err1, err2)
	}
	if ec != ed {
		t.Errorf("composed İ and decomposed I+dot must agree (NFC closure): %q vs %q", ec, ed)
	}
	if ec != "ix@ex.com" {
		t.Errorf("documented frozen edge: İ lowercases to plain 'i' (dot dropped), got %q", ec)
	}
	// Determinism: same input, same bytes, always.
	if !bytes.Equal(h.EmailHash(ec), h.EmailHash(ed)) {
		t.Error("İ composed/decomposed must hash identically")
	}
}

// Trailing-dot variants all collapse to the same domain: the FQDN ASCII dot, a double dot,
// and the three Unicode label separators (U+3002/U+FF0E/U+FF61) that UTS-46 maps to '.'
// during ToASCII. Getting the strip order wrong (before ToASCII) splits the CJK forms.
func TestTrailingDotVariantsCollapse(t *testing.T) {
	h := newTestHasher(t)
	forms := []string{
		"user@example.com",
		"user@example.com.",
		"user@example.com..",
		"user@example.com。", // 。 ideographic full stop
		"user@example.com．", // ． fullwidth full stop
		"user@example.com｡", // ｡ halfwidth ideographic full stop
	}
	var want []byte
	for i, f := range forms {
		e, d, err := h.Normalize(f)
		if err != nil {
			t.Fatalf("Normalize(%q): %v", f, err)
		}
		if d != "example.com" {
			t.Errorf("domain must collapse to example.com, got %q for %q", d, f)
		}
		if i == 0 {
			want = h.EmailHash(e)
		} else if !bytes.Equal(h.EmailHash(e), want) {
			t.Errorf("trailing-dot form %q must hash identically to the bare form", f)
		}
	}
}

// A gmail alias that folds to an empty local ("+tag@gmail.com", ".@gmail.com") must yield
// ok=false (store NULL) — never a degenerate shared "@gmail.com" canonical key.
func TestProviderCanonicalEmptyLocalRejected(t *testing.T) {
	h := newTestHasher(t)
	for _, in := range []string{"+tag@gmail.com", ".@gmail.com"} {
		// These normalize fine; the canonical fold is what empties the local.
		n, _, err := h.Normalize(in)
		if err != nil {
			continue // if it doesn't even normalize, there is no canonical to guard
		}
		if _, ok := h.ProviderCanonicalHash(n, "google"); ok {
			t.Errorf("folded-empty gmail local %q must yield ok=false (NULL), not a degenerate key", in)
		}
	}
}

// A Workspace-style custom domain classified "google" must NOT be dot-folded (§3.3.5 trap):
// only literal gmail.com/googlemail.com alias. Its canonical must equal its primary.
func TestProviderCanonicalWorkspaceNotFolded(t *testing.T) {
	h := newTestHasher(t)
	n, _, _ := h.Normalize("jane.doe@acmecorp.com") // a google-Workspace custom domain
	c, ok := h.ProviderCanonicalHash(n, "google")
	if !ok || !bytes.Equal(c, h.EmailHash(n)) {
		t.Error("google-classified custom domain must NOT fold dots (only literal gmail): canonical must equal primary")
	}
}
