// Package idgen produces time-ordered UUIDv7 identifiers (RFC 9562 §5.7) for the LOG's
// primary key (P1 §3.2.2). The id is a FROZEN join anchor — send_outcomes links to it
// (Door 2 / §4.7) — so it is generated control-plane-side and kept locality-friendly:
// the leading 48 bits are a Unix-ms timestamp, so append inserts stay index-adjacent
// (the §3.2.2 rationale for v7 over random v4).
//
// Implemented in-package (not a dependency) because it is tiny, security-relevant (the
// random bits use crypto/rand), and load-bearing enough to want under our own test.
package idgen

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"time"
)

// NewV7 returns a UUIDv7 as its 16 raw bytes, using the given time for the timestamp
// prefix. Layout (RFC 9562 §5.7): 48-bit big-endian unix_ts_ms | ver(0x7) | 12 rand bits
// | var(0b10) | 62 rand bits. Monotonic-within-ms is not required for our use (the LOG
// tolerates equal-ms ids; uniqueness comes from the 74 random bits), so we keep it simple.
func NewV7(now time.Time) ([16]byte, error) {
	var b [16]byte
	ms := now.UnixMilli()
	if ms < 0 {
		return b, fmt.Errorf("idgen: refusing a pre-epoch timestamp (%d ms)", ms)
	}
	// 48-bit timestamp in the top 6 bytes (big-endian).
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(ms))
	copy(b[0:6], ts[2:8])

	// 74 random bits fill bytes 6..15 (the version/variant nibbles are overwritten next).
	if _, err := rand.Read(b[6:]); err != nil {
		return b, fmt.Errorf("idgen: crypto/rand failed: %w", err)
	}
	b[6] = 0x70 | (b[6] & 0x0f) // version 7 in the high nibble of byte 6
	b[8] = 0x80 | (b[8] & 0x3f) // variant 0b10 in the high bits of byte 8
	return b, nil
}

// NewV7String returns a canonical 8-4-4-4-12 hyphenated UUIDv7 string.
func NewV7String(now time.Time) (string, error) {
	b, err := NewV7(now)
	if err != nil {
		return "", err
	}
	return format(b), nil
}

func format(b [16]byte) string {
	var buf [36]byte
	hex.Encode(buf[0:8], b[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], b[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], b[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], b[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], b[10:16])
	return string(buf[:])
}
