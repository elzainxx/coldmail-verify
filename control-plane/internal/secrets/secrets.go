// Package secrets is the secrets-handling scaffold. It LOADS credentials from the
// environment / secure config at startup and holds them in memory only — nothing here
// is ever written to disk, logged, or committed. (PROJECT_RULES.md secrets custody.)
//
// The four secret classes (config-manifest.md §1.1 / §8.3):
//   - pepper            the global HMAC-SHA256 email-hash pepper (Door 3)
//   - node tokens       control-plane -> verifier-node auth (per-node, referenced by id)
//   - vendor keys       control-plane -> third-party verifier (P6)
//   - supabase key      the service-role key gating all state
//
// ── THE PEPPER CUSTODY NOTE (the most permanent secret in the system) ───────────────
// The pepper is chosen ONCE and can never change in place (P1 §3.3.3, OQ-1 ELEVATED).
//   LOSE it  -> every historical email_hash de-joins; cache/dedup/flywheel linkage is
//              uncomputable FOREVER (raw addresses were never stored). No recovery.
//   LEAK it  -> low-entropy local-parts become brute-forceable; a full-address breach.
// Therefore it gets its OWN tested backup/custody runbook (not a line in a secrets
// manager): generated once with strong entropy; stored OUTSIDE the DB; redundant,
// geographically-separate, access-audited backups; recovery drilled BEFORE P1 ships;
// rotation is additive-only ("assume never rotated"). The control-plane holds it in memory to
// compute HMACs, so control-plane-host hardening + core-dump suppression are part of custody
// (cross-phase F-9/F-19, pre-takeoff-checklist §1.1). This scaffold LOADS it; the
// runbook + generation + backup are an operational artifact, gated before P1 code.
package secrets

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Secrets holds the loaded runtime secrets. Never serialize this type.
type Secrets struct {
	pepper       []byte            // Door 3 — see custody note above.
	nodeTokens   map[string]string // node id -> token (control-plane->verifier-node). Never stored in the nodes table in clear.
	vendorKeys   map[string]string // vendor name -> API key (P6). Layer stays disabled until a key + DPA exist.
	supabaseKey  string            // service-role key (P1). The sole credential gating all state.
	frontDoorKey string            // caller -> control-plane bearer (P1; per-caller + rotation is a P5 gate, PN-13).

	// queueAddrKey is the P5 DR-4 at-rest encryption key for the work_items plaintext-address
	// column (ADR-0002). It is a SEPARATE secret from the pepper, DELIBERATELY (F-9/F-19
	// crown-jewel separation) — never derived from it, never stored with it. See the custody note
	// on queue.AddrCipher: losing this key only makes in-flight (not-yet-settled) work
	// undecryptable (recoverable), unlike losing the pepper (all history de-joins forever). It
	// CAN rotate: queueAddrKeyPrev holds the previous key during a two-key overlap so a decrypt
	// tries current-then-previous while items re-seal or drain (bounded by max_retry_age).
	queueAddrKey     []byte // AES-256 (32 bytes); P5 queue subsystem refuses to start without it
	queueAddrKeyPrev []byte // optional previous key during rotation overlap (may be empty)
}

// Source abstracts where secrets come from so tests inject fakes and prod reads env
// (or a mounted secrets file). Phase 0 ships the env source; a file/KMS source slots
// in behind this interface without touching callers.
type Source interface {
	Get(key string) (string, bool)
}

// EnvSource reads from the process environment (12-factor). In real deployment these
// env vars are populated from a secrets manager / mounted file, NEVER from committed
// config (see .gitignore). Phase 0's docker-compose supplies only non-secret placeholders.
type EnvSource struct{}

func (EnvSource) Get(key string) (string, bool) { return os.LookupEnv(key) }

// Canonical env var names (documented in config/secrets.example.env).
const (
	EnvPepper       = "TM_PEPPER"          // hex or base64; the Door-3 pepper
	EnvSupabaseKey  = "TM_SUPABASE_KEY"    // service-role key
	EnvFrontDoorKey = "TM_FRONTDOOR_TOKEN" // caller->control-plane bearer
	EnvNodeTokens   = "TM_NODE_TOKENS"     // "nodeid=token,nodeid2=token2"
	EnvVendorKeys   = "TM_VENDOR_KEYS"     // "millionverifier=key,other=key" (P6)

	// P5 DR-4 (ADR-0002): the work_items plaintext-address at-rest encryption key. SEPARATE from
	// the pepper. Hex or base64, decoding to exactly 32 bytes (AES-256). _PREV holds the previous
	// key during a rotation overlap (optional). See the queue.AddrCipher custody note.
	EnvQueueAddrKey     = "TM_QUEUE_ADDR_KEY"      // AES-256-GCM key for work_items.addr_ciphertext
	EnvQueueAddrKeyPrev = "TM_QUEUE_ADDR_KEY_PREV" // previous key during rotation overlap (optional)
)

// Load reads all secrets from the source. It is STRICT about the pepper (Door 3) and
// the Supabase key (all state) because a control-plane that starts without them is silently
// broken; other secrets may be absent at Phase 0 (their features are stubbed/off).
//
// requireCore=false lets Phase-0 build/smoke run without real secrets present; set it
// true for any real (feature-serving) startup so the fragile secrets can't be missing.
func Load(src Source, requireCore bool) (*Secrets, error) {
	s := &Secrets{
		nodeTokens: map[string]string{},
		vendorKeys: map[string]string{},
	}

	if v, ok := src.Get(EnvPepper); ok && v != "" {
		s.pepper = []byte(v) // decoding (hex/base64) is a P1 wiring detail; kept as bytes here.
	} else if requireCore {
		return nil, fmt.Errorf("secrets: %s is required (Door 3 pepper — refusing to start without it)", EnvPepper)
	}

	if v, ok := src.Get(EnvSupabaseKey); ok {
		s.supabaseKey = v
	} else if requireCore {
		return nil, fmt.Errorf("secrets: %s is required (the credential gating all state)", EnvSupabaseKey)
	}

	if v, ok := src.Get(EnvFrontDoorKey); ok {
		s.frontDoorKey = v
	}
	// P5 DR-4: the work_items at-rest key (+ optional previous key for rotation). Decoded to raw
	// bytes here (hex/base64); the LENGTH is validated fail-closed by queue.NewAddrCipher (must be
	// 32 bytes). Not required by requireCore — the QUEUE SUBSYSTEM refuses to start without it
	// (honest not-ready), so a non-queue deployment need not set it.
	if v, ok := src.Get(EnvQueueAddrKey); ok && v != "" {
		s.queueAddrKey = decodeKey(v)
	}
	if v, ok := src.Get(EnvQueueAddrKeyPrev); ok && v != "" {
		s.queueAddrKeyPrev = decodeKey(v)
	}
	s.nodeTokens = parsePairs(src, EnvNodeTokens)
	s.vendorKeys = parsePairs(src, EnvVendorKeys)
	return s, nil
}

// decodeKey decodes a key string as hex, then base64, falling back to the raw bytes. The caller
// (queue.NewAddrCipher) enforces the 32-byte AES-256 length fail-closed, so a mis-encoded value
// simply fails there rather than silently weakening the cipher.
func decodeKey(v string) []byte {
	if b, err := hex.DecodeString(v); err == nil && len(b) == 32 {
		return b
	}
	if b, err := base64.StdEncoding.DecodeString(v); err == nil && len(b) == 32 {
		return b
	}
	return []byte(v)
}

func parsePairs(src Source, key string) map[string]string {
	out := map[string]string{}
	v, ok := src.Get(key)
	if !ok || v == "" {
		return out
	}
	for _, pair := range strings.Split(v, ",") {
		if k, val, found := strings.Cut(strings.TrimSpace(pair), "="); found {
			out[strings.TrimSpace(k)] = strings.TrimSpace(val)
		}
	}
	return out
}

// Pepper returns the Door-3 pepper, or an error if it was not loaded. Callers must not
// cache the returned slice beyond what they need; it is the system's most fragile secret.
func (s *Secrets) Pepper() ([]byte, error) {
	if len(s.pepper) == 0 {
		return nil, errors.New("secrets: pepper not loaded")
	}
	return s.pepper, nil
}

// HasPepper reports whether a pepper was loaded (for readiness checks without exposing it).
func (s *Secrets) HasPepper() bool     { return len(s.pepper) > 0 }
func (s *Secrets) HasSupabaseKey() bool { return s.supabaseKey != "" }

// QueueAddrKey returns the P5 DR-4 at-rest key (+ any previous rotation key), for the queue
// subsystem to build a queue.AddrCipher. ok=false when no key is loaded (the queue subsystem then
// refuses to start rather than storing plaintext). The previous key is empty when no rotation is
// in progress. The returned slices are the loaded bytes — the caller passes them to
// NewAddrCipher, which enforces the 32-byte length fail-closed.
func (s *Secrets) QueueAddrKey() (key []byte, prev []byte, ok bool) {
	if len(s.queueAddrKey) == 0 {
		return nil, nil, false
	}
	return s.queueAddrKey, s.queueAddrKeyPrev, true
}

// HasQueueAddrKey reports whether a DR-4 key was loaded (for readiness without exposing it).
func (s *Secrets) HasQueueAddrKey() bool { return len(s.queueAddrKey) > 0 }

// NodeToken / VendorKey return a referenced secret by id, or false if absent.
func (s *Secrets) NodeToken(nodeID string) (string, bool) { v, ok := s.nodeTokens[nodeID]; return v, ok }
func (s *Secrets) VendorKey(vendor string) (string, bool) { v, ok := s.vendorKeys[vendor]; return v, ok }

// FrontDoorToken returns the caller->control-plane bearer for comparison at the front door.
// Unlike the pepper (which never leaves this package), the front-door token must be
// compared against the request header, so it is exposed narrowly here.
func (s *Secrets) FrontDoorToken() (string, bool) {
	if s.frontDoorKey == "" {
		return "", false
	}
	return s.frontDoorKey, true
}

// String deliberately redacts everything — a Secrets value must never leak into logs.
func (s *Secrets) String() string {
	return fmt.Sprintf("Secrets{pepper:%s supabase:%s frontdoor:%s queueAddr:%s queueAddrPrev:%s nodeTokens:%d vendorKeys:%d}",
		redact(len(s.pepper) > 0), redact(s.supabaseKey != ""), redact(s.frontDoorKey != ""),
		redact(len(s.queueAddrKey) > 0), redact(len(s.queueAddrKeyPrev) > 0),
		len(s.nodeTokens), len(s.vendorKeys))
}

func redact(present bool) string {
	if present {
		return "<set>"
	}
	return "<unset>"
}
