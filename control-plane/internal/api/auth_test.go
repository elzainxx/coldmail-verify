package api

import (
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mnfi/codmailling/control-plane/internal/config"
	"github.com/mnfi/codmailling/control-plane/internal/secrets"
	"github.com/mnfi/codmailling/control-plane/internal/store"
)

// authReq builds a request with an optional bearer.
func authReq(bearer string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/verify", strings.NewReader(`{"email":"a@b.co"}`))
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	return r
}

func sha(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

// fixedNow makes the validity-window checks deterministic.
func serverAtTime(t *testing.T, mem *store.MemStore, staticTok string, now time.Time) *Server {
	t.Helper()
	cfg := config.Defaults()
	cfg.Env = "production" // enforce auth
	src := secrets.Source(fakeSource{})
	if staticTok != "" {
		src = fakeSource{secrets.EnvFrontDoorKey: staticTok}
	}
	sec, err := secrets.Load(src, false)
	if err != nil {
		t.Fatalf("secrets: %v", err)
	}
	s := New(cfg, sec, mustHasher(t)).WithCallerAuth(mem)
	s.now = func() time.Time { return now }
	return s
}

// TestAuthOK_PerCallerKeyLifecycle is the PN-13 fail-verify: valid active key authenticates;
// unknown key rejected; rotated-out key rejected; during a rotation overlap BOTH keys authenticate;
// and the legacy static token works on sync but is rejected on the bulk (per-caller-required)
// surface while a per-caller key is accepted there.
//
// FAIL-VERIFY (pins the load-bearing guards):
//   - remove the `now ∈ [not_before, not_after)` window from LookupCaller → the rotated-out case
//     starts authenticating → this test fails (proves expiry is enforced, not cosmetic).
//   - make the bulk path call authenticate(r, false) instead of true → the "static token rejected
//     on bulk" case fails (proves the F-20 bulk gate is load-bearing).
func TestAuthOK_PerCallerKeyLifecycle(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	mem := store.NewMem(func() time.Time { return now })

	// Seed: an active key (no expiry), and a rotated-out key (not_after in the past).
	mem.SeedCaller(sha("active-key"), "caller_active", now.Add(-time.Hour), time.Time{})
	mem.SeedCaller(sha("expired-key"), "caller_active", now.Add(-48*time.Hour), now.Add(-time.Hour))
	// Rotation overlap: caller_rot has TWO live keys (old expiring soon, new no-expiry).
	mem.SeedCaller(sha("rot-old"), "caller_rot", now.Add(-24*time.Hour), now.Add(time.Hour))
	mem.SeedCaller(sha("rot-new"), "caller_rot", now, time.Time{})

	s := serverAtTime(t, mem, "legacy-static", now)

	// (1) valid active key → authenticated, correct caller_id (sync surface).
	if id, ok := s.authenticate(authReq("active-key"), false); !ok || id != "caller_active" {
		t.Fatalf("active key: ok=%v id=%q, want true/caller_active", ok, id)
	}
	// (2) unknown key → rejected.
	if _, ok := s.authenticate(authReq("nope"), false); ok {
		t.Fatal("unknown key authenticated")
	}
	// (3) rotated-out key (not_after in the past) → rejected.
	if _, ok := s.authenticate(authReq("expired-key"), false); ok {
		t.Fatal("expired/rotated-out key authenticated — window not enforced")
	}
	// (4) rotation overlap: BOTH old and new authenticate to the same caller.
	if id, ok := s.authenticate(authReq("rot-old"), false); !ok || id != "caller_rot" {
		t.Fatalf("rotation old key: ok=%v id=%q", ok, id)
	}
	if id, ok := s.authenticate(authReq("rot-new"), false); !ok || id != "caller_rot" {
		t.Fatalf("rotation new key: ok=%v id=%q", ok, id)
	}
	// (5) legacy static token: accepted on sync (requirePerCaller=false)...
	if _, ok := s.authenticate(authReq("legacy-static"), false); !ok {
		t.Fatal("legacy static token rejected on sync — backward-compat broken")
	}
	// ...but REJECTED on the bulk-PII surface (requirePerCaller=true).
	if _, ok := s.authenticate(authReq("legacy-static"), true); ok {
		t.Fatal("F-20 VIOLATED: legacy static token accepted on the per-caller-required (bulk) surface")
	}
	// ...while a per-caller key IS accepted on the bulk surface.
	if id, ok := s.authenticate(authReq("active-key"), true); !ok || id != "caller_active" {
		t.Fatalf("per-caller key on bulk surface: ok=%v id=%q", ok, id)
	}
}

// TestAuthOK_NotYetValidKeyRejected proves not_before is enforced (a key issued for the future).
func TestAuthOK_NotYetValidKeyRejected(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	mem := store.NewMem(func() time.Time { return now })
	mem.SeedCaller(sha("future-key"), "caller_future", now.Add(time.Hour), time.Time{})
	s := serverAtTime(t, mem, "", now)
	if _, ok := s.authenticate(authReq("future-key"), false); ok {
		t.Fatal("not-yet-valid key authenticated — not_before not enforced")
	}
}

// TestAuthOK_WrongStaticTokenRejected proves the constant-time static compare rejects a wrong token
// even when caller-auth is wired (the miss falls through to the static check, which must reject).
func TestAuthOK_WrongStaticTokenRejected(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	mem := store.NewMem(func() time.Time { return now })
	s := serverAtTime(t, mem, "the-real-static", now)
	if _, ok := s.authenticate(authReq("wrong-static"), false); ok {
		t.Fatal("wrong static token authenticated")
	}
	if _, ok := s.authenticate(authReq("the-real-static"), false); !ok {
		t.Fatal("correct static token rejected")
	}
}
