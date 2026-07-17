package idgen

import (
	"testing"
	"time"
)

func TestV7VersionAndVariant(t *testing.T) {
	b, err := NewV7(time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatalf("NewV7: %v", err)
	}
	if got := b[6] >> 4; got != 0x7 {
		t.Errorf("version nibble = %x, want 7", got)
	}
	if got := b[8] >> 6; got != 0b10 {
		t.Errorf("variant bits = %b, want 10", got)
	}
}

func TestV7TimeOrdered(t *testing.T) {
	// Later timestamp => lexicographically/byte-wise greater prefix (locality guarantee).
	early, _ := NewV7(time.UnixMilli(1_000_000))
	late, _ := NewV7(time.UnixMilli(2_000_000))
	// Compare the 48-bit timestamp prefix only (random tail may differ either way).
	for i := 0; i < 6; i++ {
		if early[i] != late[i] {
			if early[i] > late[i] {
				t.Errorf("earlier id has greater ts prefix at byte %d: %x > %x", i, early[i], late[i])
			}
			return
		}
	}
}

func TestV7Unique(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	seen := map[[16]byte]bool{}
	for i := 0; i < 10000; i++ {
		b, err := NewV7(now) // same ms — uniqueness must come from the random bits
		if err != nil {
			t.Fatalf("NewV7: %v", err)
		}
		if seen[b] {
			t.Fatalf("collision at %d within one ms", i)
		}
		seen[b] = true
	}
}

func TestV7StringFormat(t *testing.T) {
	s, err := NewV7String(time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatalf("NewV7String: %v", err)
	}
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[14] != '7' || s[18] != '-' || s[23] != '-' {
		t.Errorf("bad UUID format: %q", s)
	}
}
