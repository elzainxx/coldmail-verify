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
// (no editor/tool byte-encoding fragility): ö = 'ö' (composed), ̈ = combining
// diaeresis (the decomposed diacritic).

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

func TestNonASCIIDomainRefusedNotMishashed(t *testing.T) {
	// Phase-0 honesty: until IDNA is wired (P1), a non-ASCII domain is REFUSED, not
	// hashed un-punycoded — so no wrong hash enters the frozen store early.
	h := newTestHasher(t)
	if _, _, err := h.Normalize("user@exämple.com"); err != ErrUnparseable { // exämple.com
		t.Errorf("non-ASCII domain must be refused at P0 (IDNA is the P1 wiring point), got err=%v", err)
	}
}

// Regression (code-review finding): a non-ASCII LOCAL part must ALSO be refused until NFC
// is wired at P1 — otherwise the composed vs decomposed byte forms of the same visual
// address hash DIFFERENTLY, splitting one identity in the frozen store (a Door-3
// corruption). The original suite only covered the domain side; this pins the local side.
func TestNonASCIILocalPartRefused(t *testing.T) {
	h := newTestHasher(t)
	composed := "jöhn@example.com"    // j ö hn      (ö = one code point)
	decomposed := "jöhn@example.com" // j o ◌̈ hn   (o + combining diaeresis)
	if composed == decomposed {
		t.Fatal("test setup: forms are byte-identical")
	}
	// Both are non-ASCII and MUST be refused (the WHY: un-normalized they hash differently).
	for _, in := range []string{composed, decomposed} {
		if _, _, err := h.Normalize(in); err != ErrUnparseable {
			t.Errorf("non-ASCII local part must be refused at P0 (NFC is the P1 wiring point), got err=%v for %q", err, in)
		}
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
