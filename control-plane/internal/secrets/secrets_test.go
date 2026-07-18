package secrets

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

type fakeSource map[string]string

func (f fakeSource) Get(k string) (string, bool) { v, ok := f[k]; return v, ok }

// TestQueueAddrKey_LoadsHexBase64Raw proves the DR-4 key decodes from hex OR base64 to the raw
// 32 bytes, and the accessor reports presence. The 32-byte length is enforced downstream by
// queue.NewAddrCipher (fail-closed), so decodeKey only needs to recover the bytes.
func TestQueueAddrKey_LoadsHexBase64Raw(t *testing.T) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	cases := []struct {
		name string
		val  string
	}{
		{"hex", hex.EncodeToString(raw)},
		{"base64", base64.StdEncoding.EncodeToString(raw)},
		{"raw32", string(raw)}, // already 32 bytes literally
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Load(fakeSource{EnvQueueAddrKey: tc.val}, false)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			key, prev, ok := s.QueueAddrKey()
			if !ok {
				t.Fatal("QueueAddrKey ok=false; want a loaded key")
			}
			if len(key) != 32 {
				t.Fatalf("decoded key len=%d, want 32", len(key))
			}
			if string(key) != string(raw) {
				t.Fatalf("decoded key mismatch for %s encoding", tc.name)
			}
			if len(prev) != 0 {
				t.Fatal("prev key should be empty when _PREV unset")
			}
			if !s.HasQueueAddrKey() {
				t.Fatal("HasQueueAddrKey=false after load")
			}
		})
	}
}

// TestQueueAddrKey_Absent proves a deployment without the key reports ok=false (the queue
// subsystem then refuses to start rather than storing plaintext) — but Load itself does NOT fail
// (requireCore stays pepper+supabase only, so a non-queue deployment boots fine).
func TestQueueAddrKey_Absent(t *testing.T) {
	s, err := Load(fakeSource{}, false)
	if err != nil {
		t.Fatalf("Load with no queue key must still succeed: %v", err)
	}
	if _, _, ok := s.QueueAddrKey(); ok {
		t.Fatal("QueueAddrKey ok=true with no key set")
	}
	if s.HasQueueAddrKey() {
		t.Fatal("HasQueueAddrKey=true with no key set")
	}
}

// TestQueueAddrKey_RotationPrev proves the previous key is loaded for a rotation overlap.
func TestQueueAddrKey_RotationPrev(t *testing.T) {
	cur := hex.EncodeToString(make([]byte, 32))
	prevRaw := make([]byte, 32)
	for i := range prevRaw {
		prevRaw[i] = 0xAA
	}
	s, err := Load(fakeSource{
		EnvQueueAddrKey:     cur,
		EnvQueueAddrKeyPrev: hex.EncodeToString(prevRaw),
	}, false)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, prev, ok := s.QueueAddrKey()
	if !ok || len(prev) != 32 || string(prev) != string(prevRaw) {
		t.Fatalf("previous rotation key not loaded correctly: ok=%v len=%d", ok, len(prev))
	}
}

// TestString_RedactsQueueKey proves the DR-4 key never appears in String() (secrets never logged).
func TestString_RedactsQueueKey(t *testing.T) {
	secret := hex.EncodeToString(make([]byte, 32))
	s, _ := Load(fakeSource{EnvQueueAddrKey: secret, EnvPepper: "pep"}, false)
	str := s.String()
	if strings.Contains(str, secret) {
		t.Fatal("String() leaked the raw queue key value")
	}
	// Presence is reported (set), value is not.
	if !strings.Contains(str, "queueAddr:<set>") {
		t.Fatalf("String() should report queueAddr:<set>, got %q", str)
	}
}
