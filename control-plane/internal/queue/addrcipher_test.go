package queue

import (
	"bytes"
	"strings"
	"testing"
)

// key32 builds a deterministic 32-byte test key from a seed byte (never a real key).
func key32(seed byte) []byte {
	k := make([]byte, AddrKeyLen)
	for i := range k {
		k[i] = seed + byte(i)
	}
	return k
}

// TestAddrCipher_CiphertextContainsNoPlaintext is the DR-4 fail-verify at the unit level: the
// sealed blob must contain NEITHER the local-part NOR the full address in clear (ADR-0002's
// "dump → only ciphertext", proven locally). FAIL-VERIFY: replace Seal's body with an identity
// passthrough (`return []byte(plaintextAddr), nil`) and this test fails on the substring scan —
// proving the AEAD is the load-bearing guard, not incidental.
func TestAddrCipher_CiphertextContainsNoPlaintext(t *testing.T) {
	c, err := NewAddrCipher(key32(1))
	if err != nil {
		t.Fatalf("NewAddrCipher: %v", err)
	}
	const addr = "secretlocal@acmecorp.com"
	aad := []byte("work-item-id-abc")
	blob, err := c.Seal(addr, aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// The at-rest bytes must not contain the local-part or the full address (same spirit as the
	// ADR-0008 redaction assertions in smtp_stage_test.go).
	if bytes.Contains(blob, []byte("secretlocal")) {
		t.Fatal("DR-4 VIOLATED: sealed blob contains the plaintext local-part")
	}
	if bytes.Contains(blob, []byte(addr)) {
		t.Fatal("DR-4 VIOLATED: sealed blob contains the full plaintext address")
	}
	// It must round-trip back to the exact address under the same AAD.
	got, err := c.Open(blob, aad)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got != addr {
		t.Fatalf("round-trip mismatch: got %q want %q", got, addr)
	}
}

// TestAddrCipher_NonceIsRandomized proves the same address seals to DIFFERENT bytes each call (a
// fresh random nonce), so the ciphertext is not an equality oracle on the address.
func TestAddrCipher_NonceIsRandomized(t *testing.T) {
	c, _ := NewAddrCipher(key32(2))
	aad := []byte("row-1")
	b1, _ := c.Seal("jane@example.com", aad)
	b2, _ := c.Seal("jane@example.com", aad)
	if bytes.Equal(b1, b2) {
		t.Fatal("two seals of the same address produced identical bytes — nonce not randomized")
	}
	// Both still open to the same plaintext.
	for _, b := range [][]byte{b1, b2} {
		if got, err := c.Open(b, aad); err != nil || got != "jane@example.com" {
			t.Fatalf("open mismatch: got %q err %v", got, err)
		}
	}
}

// TestAddrCipher_AADBindsToRow is the anti-replay guard: a blob sealed under one row's id must NOT
// open under another row's id. FAIL-VERIFY: pass nil AAD to both Seal and Open and this
// cross-row Open would succeed — proving the AAD binding is load-bearing.
func TestAddrCipher_AADBindsToRow(t *testing.T) {
	c, _ := NewAddrCipher(key32(3))
	blob, err := c.Seal("victim@corp.com", []byte("row-A"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// Opening under a DIFFERENT row id must fail (a moved/replayed ciphertext is rejected).
	if _, err := c.Open(blob, []byte("row-B")); err == nil {
		t.Fatal("anti-replay VIOLATED: a blob sealed for row-A opened under row-B")
	}
	// Opening under the correct row id still works.
	if got, err := c.Open(blob, []byte("row-A")); err != nil || got != "victim@corp.com" {
		t.Fatalf("correct-AAD open failed: got %q err %v", got, err)
	}
}

// TestAddrCipher_TamperedBlobRejected proves GCM authentication: flipping any byte of the sealed
// blob makes Open fail (never returns a guessed/garbled plaintext).
func TestAddrCipher_TamperedBlobRejected(t *testing.T) {
	c, _ := NewAddrCipher(key32(4))
	aad := []byte("row-x")
	blob, _ := c.Seal("a@b.com", aad)
	tampered := bytes.Clone(blob)
	tampered[len(tampered)-1] ^= 0x01 // flip a bit in the tag
	if _, err := c.Open(tampered, aad); err == nil {
		t.Fatal("tampered ciphertext opened without error — GCM auth not enforced")
	}
}

// TestAddrCipher_RotationOverlap proves the two-key rotation posture: a blob sealed under a
// PREVIOUS key still opens after the primary rotates, because Open tries primary-then-previous.
func TestAddrCipher_RotationOverlap(t *testing.T) {
	oldKey, newKey := key32(5), key32(9)
	// Seal under the OLD key (as it would have been before rotation).
	oldCipher, _ := NewAddrCipher(oldKey)
	aad := []byte("row-rot")
	blob, _ := oldCipher.Seal("legacy@corp.com", aad)

	// After rotation: primary = newKey, previous = oldKey. Open must still succeed via the previous.
	rotated, err := NewAddrCipher(newKey, oldKey)
	if err != nil {
		t.Fatalf("NewAddrCipher(rotation): %v", err)
	}
	if got, err := rotated.Open(blob, aad); err != nil || got != "legacy@corp.com" {
		t.Fatalf("rotation-overlap open failed: got %q err %v", got, err)
	}
	// A cipher with ONLY the new key must NOT open the old blob (proves the overlap is what saved it).
	newOnly, _ := NewAddrCipher(newKey)
	if _, err := newOnly.Open(blob, aad); err == nil {
		t.Fatal("old blob opened under the new key alone — overlap test is not actually exercising rotation")
	}
}

// TestAddrCipher_RefusesBadKeyLength is the fail-closed guard: a non-32-byte key is rejected at
// construction (mirrors hash.NewHasher refusing an empty pepper). A queue-enabled brain with a
// bad key is honestly not-ready, never silently degrading to a weak/no cipher.
func TestAddrCipher_RefusesBadKeyLength(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33, 64} {
		if _, err := NewAddrCipher(make([]byte, n)); err == nil {
			t.Fatalf("NewAddrCipher accepted a %d-byte key (must require %d)", n, AddrKeyLen)
		}
	}
	// A bad PREVIOUS key is also rejected (rotation must not silently drop a malformed key).
	if _, err := NewAddrCipher(key32(1), make([]byte, 10)); err == nil {
		t.Fatal("NewAddrCipher accepted a malformed previous key")
	}
	// An EMPTY previous key is allowed (rotation not in progress → skipped), not an error.
	if _, err := NewAddrCipher(key32(1), nil); err != nil {
		t.Fatalf("NewAddrCipher rejected an absent previous key: %v", err)
	}
}

// TestAddrCipher_OpenShortBlob guards the length check (a truncated blob is rejected, not panicked).
func TestAddrCipher_OpenShortBlob(t *testing.T) {
	c, _ := NewAddrCipher(key32(7))
	if _, err := c.Open([]byte{0x01, 0x02}, []byte("row")); err == nil || !strings.Contains(err.Error(), "too short") {
		t.Fatalf("expected too-short error, got %v", err)
	}
}
