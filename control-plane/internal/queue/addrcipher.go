// Package queue is the Phase-5 durable async work queue (spec §3): the brain-side store of
// pending verification work-items, drained through the UNCHANGED Phase-4 router (the master
// invariant §8 — the queue changes WHEN a check runs, never WHETHER the safeguards apply).
//
// This file is the DR-4 gate: at-rest encryption of the ONE plaintext-at-rest exception.
//
// ── THE work_items PLAINTEXT EXCEPTION AND ITS KEY (DR-4 / ADR-0002) ─────────────────────────
// SMTP structurally requires the REAL address at attempt time and cannot run from a hash, so the
// queue must hold the plaintext address for work-not-yet-done — the ONE deliberate, bounded
// exception to Door 3's hash-everything discipline (spec §3.3, OQ-P5-9). The exception is bounded
// two ways: (1) purged transactionally on settle (§5.3 step 4 — never a later sweep, so a crash
// can't strand it), and (2) encrypted at rest by THIS cipher (DR-4 — so a DB backup/PITR snapshot,
// which no purge transaction ever reaches, holds only ciphertext, closing the ADR-0002 gap).
//
// THE KEY IS A SEPARATE SECRET FROM THE PEPPER — deliberately (F-9/F-19 crown-jewel separation):
//   - Two secrets, two blast radii, NEVER derived from each other or stored together.
//   - LOSE the pepper → ALL history de-joins forever (irrecoverable). LOSE this key → only
//     in-flight, not-yet-settled work is undecryptable (recoverable: re-enqueue; settled work
//     never needed it).
//   - LEAK the pepper → low-entropy addresses brute-forceable across ALL history. LEAK this key →
//     only the plaintext of items live at snapshot time is exposed.
//   - So a snapshot leak ≠ a pepper leak and vice-versa (ADR-0002 §Consequences). Collapsing them
//     into one secret would re-concentrate exactly the crown jewels F-9/F-19 warns against.
// Unlike the pepper ("assume never rotated"), THIS key CAN rotate: items are short-lived (bounded
// by max_retry_age, §4.5), and a two-key overlap (primary + previous) lets Open try current-then-
// previous while callers re-seal or simply drain. The rotation posture is recorded in the P5
// data-handling runbook (ADR-0002 gate, pre-takeoff §3.7).
//
// RESIDUAL, stated honestly (P5 runbook): the brain process holds this key in memory AND reads the
// ciphertext AND holds the plaintext transiently to run SMTP — a brain compromise / core dump
// exposes key + plaintext together (the same F-9/F-19 concentration the pepper has). Encryption
// does NOT defend a compromised brain; it defends the AT-REST / BACKUP surface. Core-dump
// suppression + host-hardening (PN-12) is the mitigation, named alongside pepper custody.
package queue

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// AddrKeyLen is the required key length: AES-256 (32 bytes). NewAddrCipher refuses any other length
// (fail-closed — mirrors hash.NewHasher refusing a zero-length pepper). The queue subsystem
// constructor refuses to start without a valid key, so a queue-enabled brain never silently stores
// plaintext: it is honestly not-ready instead.
const AddrKeyLen = 32

// AddrCipher seals/opens the work_items plaintext-address column with AES-256-GCM (AEAD). The
// stored blob is nonce‖ciphertext‖tag (the nonce is prepended — the standard Go idiom; the nonce
// is not a secret). GCM authenticates the ciphertext AND its associated data (AAD): callers pass
// the work_item id as AAD so a sealed blob CANNOT be lifted from one row and replayed into another
// — a moved/tampered ciphertext fails Open.
//
// It holds a PRIMARY cipher (used for Seal) and an ordered list of ciphers (primary first, then
// any previous keys) tried in order by Open — the two-key rotation overlap.
type AddrCipher struct {
	seal cipher.AEAD   // primary — Seal always uses this (the current key)
	open []cipher.AEAD // primary + optional previous keys — Open tries each in order (rotation)
}

// NewAddrCipher builds the cipher from the primary key plus zero or more previous keys (for
// rotation overlap). Every key must be exactly AddrKeyLen bytes; a wrong length is a fail-closed
// error (never a weak/degenerate cipher). Seal uses the primary; Open tries primary then previous.
func NewAddrCipher(primaryKey []byte, previousKeys ...[]byte) (*AddrCipher, error) {
	primary, err := newGCM(primaryKey)
	if err != nil {
		return nil, fmt.Errorf("queue: primary addr key: %w", err)
	}
	c := &AddrCipher{seal: primary, open: []cipher.AEAD{primary}}
	for i, pk := range previousKeys {
		if len(pk) == 0 {
			continue // an unset previous key (rotation not in progress) is fine — skip it
		}
		prev, err := newGCM(pk)
		if err != nil {
			return nil, fmt.Errorf("queue: previous addr key #%d: %w", i+1, err)
		}
		c.open = append(c.open, prev)
	}
	return c, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != AddrKeyLen {
		return nil, fmt.Errorf("key must be %d bytes (AES-256), got %d", AddrKeyLen, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal encrypts the plaintext address, binding it to aad (the work_item id). It returns
// nonce‖ciphertext‖tag; store that blob in work_items.addr_ciphertext. A fresh random nonce per
// call (crypto/rand) means the same address seals to different bytes each time — no equality
// oracle on the ciphertext. The nonce is stored WITH the ciphertext (it is not a secret).
func (c *AddrCipher) Seal(plaintextAddr string, aad []byte) ([]byte, error) {
	nonce := make([]byte, c.seal.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("queue: seal nonce: %w", err)
	}
	// Seal appends the ciphertext+tag to dst (here nonce) → the result is nonce‖ct‖tag.
	return c.seal.Seal(nonce, nonce, []byte(plaintextAddr), aad), nil
}

// Open decrypts a sealed blob, verifying it against aad (the same work_item id used to Seal). It
// tries the primary key then any previous keys (rotation overlap). On authentication failure under
// ALL keys it returns an error — NEVER a guessed plaintext (harden-toward-unknown at the crypto
// boundary too). A blob whose aad doesn't match (wrong row) fails Open — the anti-replay guard.
func (c *AddrCipher) Open(blob []byte, aad []byte) (string, error) {
	if len(c.open) == 0 {
		return "", errors.New("queue: no addr key configured")
	}
	nonceSize := c.seal.NonceSize()
	if len(blob) < nonceSize {
		return "", errors.New("queue: sealed blob too short")
	}
	nonce, ct := blob[:nonceSize], blob[nonceSize:]
	var lastErr error
	for _, aead := range c.open {
		pt, err := aead.Open(nil, nonce, ct, aad)
		if err == nil {
			return string(pt), nil
		}
		lastErr = err
	}
	return "", fmt.Errorf("queue: open failed under all keys: %w", lastErr)
}
