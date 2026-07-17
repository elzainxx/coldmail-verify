// Package config loads the control-plane's runtime configuration. It wires the
// config-manifest.md flags/dials as REAL, READABLE settings from day one — even
// where the feature a flag gates is still a stub. (Phase-0 rule: everything the
// manifest calls a flag is a config entry, never hardcoded.)
//
// Design:
//   - LOCKED contracts (Door 1..5) are NOT config — they live in code (vocab, hash).
//     A "flag" that would let you turn a frozen door off does not exist here by design.
//   - DIALs and feature-flags from the manifest are fields here, each with the manifest's
//     documented default (or explicitly Unset when the manifest says "owner OQ").
//   - The kill-switch ladder (kill-switch-ladder.md) and shadow-mode (shadow-mode.md)
//     are first-class config: every rung / mode is a readable setting with a SAFE default,
//     so the "cheap-layers-only" rung actually works at Phase 0 (the only rung reachable
//     without SMTP).
//
// Sources of values (highest precedence last): built-in safe defaults -> config file
// (YAML/JSON, added at P1) -> environment overrides. Phase 0 ships defaults + env so the
// skeleton is runnable and testable with zero external files.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ── Kill-switch ladder (kill-switch-ladder.md) ──────────────────────────────────────
// Each rung is a readable flag. Defaults put the system at FULL capability EXCEPT that,
// at Phase 0, no SMTP infrastructure exists — so the effective reachable state is
// CHEAP-LAYERS-ONLY (Rung 3), which the manifest calls "Phase 1's steady state". The
// flags below make that rung real and pullable by config, not code (Door 5 discipline).

// SMTPMode is Rung 1 (SMTP-off) at global scope. Per-node/per-provider scoping is later.
type SMTPMode string

const (
	SMTPEnabled  SMTPMode = "enabled"  // SMTP probing allowed on eligible nodes (P2+ real; P0: no nodes exist)
	SMTPDisabled SMTPMode = "disabled" // Rung 1 global: cheap + fallback only
)

// FallbackMode is Rung 2 (fallback-off) + the Global-A panic button.
type FallbackMode string

const (
	FallbackEnabled FallbackMode = "enabled"
	FallbackOff     FallbackMode = "off" // Rung 2 global / global-fallback-kill (PN-16; rides DR-3 audit)
)

// FrontDoorMode is Rung 4 (paused): accept-and-defer vs reject vs serving.
type FrontDoorMode string

const (
	FrontDoorServing FrontDoorMode = "serving"
	FrontDoorDefer   FrontDoorMode = "accept_and_defer" // queue-only (P5+)
	FrontDoorReject  FrontDoorMode = "reject"
)

// KillSwitch is the operational degradation ladder as config.
type KillSwitch struct {
	SMTP      SMTPMode      // Rung 1
	Fallback  FallbackMode  // Rung 2 / Global A
	FrontDoor FrontDoorMode // Rung 4
	// Rung 3 (cheap-only) is the DERIVED state SMTP=disabled AND Fallback=off; see CheapLayersOnly().
}

// CheapLayersOnly reports whether the ladder is at Rung 3 — the only rung reachable
// without SMTP infrastructure, and Phase 1's designed steady state. Phase-0 verification
// asserts this rung actually works from config alone.
func (k KillSwitch) CheapLayersOnly() bool {
	return k.SMTP == SMTPDisabled && k.Fallback == FallbackOff
}

// ── Shadow-mode (shadow-mode.md, DR-2) ──────────────────────────────────────────────
// Every data-driven mutator carries a mode; the four day-one mutators DEFAULT to
// recommend-only (never auto-mutate routing/confidence/spend without a human). This is
// a config surface at Phase 0; the mechanisms themselves are later-phase stubs.

type Mode string

const (
	ModeShadow        Mode = "shadow"         // compute, log only; never emit an actionable proposal
	ModeRecommendOnly Mode = "recommend_only" // DEFAULT for the four mutators; propose, human applies
	ModeAuto          Mode = "auto"           // mutate within the mechanism's own guards
)

// ShadowModes holds the per-mechanism mode dial. Defaults per DR-2 / shadow-mode §2.
type ShadowModes struct {
	A1BlockRateQuarantine Mode // P4 block-rate (statistical trigger). recommend-only by default.
	A6ConfidenceRecalib   Mode // P7 recalibration (sharpest). recommend-only.
	A8ProviderPromotion   Mode // P7 provider auto-promote to :mx. recommend-only.
	A9FallbackWeaning     Mode // P7 per-provider wean. recommend-only.
	// A2/A3/A4/A5/A7/A10/A11 stay auto/alert per shadow-mode §3 — not owner dials here.
}

// ── DIALs from the manifest (values tunable; existence locked) ───────────────────────
// Unset means the manifest deliberately left it an owner OQ (config-manifest §8.2); the
// zero value is NOT silently substituted for a real threshold — features read Unset and
// refuse to act on an unset critical dial rather than guessing.

// Dial is an optional int dial: Set distinguishes "0" from "owner has not chosen".
type Dial struct {
	Value int
	Set   bool
}

func (d Dial) String() string {
	if !d.Set {
		return "UNSET(owner-OQ)"
	}
	return strconv.Itoa(d.Value)
}

// Dials groups the manifest's numeric dials that a Phase-0 reader should see wired.
// (Illustrative subset — the FULL registry is Docs/config-reference.md, generated from
// the manifest. Every one here is readable and defaulted-or-explicitly-unset.)
type Dials struct {
	CacheTTLSeconds        Dial // P1 OQ-9: expires_at value. UNSET (owner). NON-NULL required at write time.
	RawBlobRetentionDays   int  // P1 OQ-10: ~90d default.
	RecommendationSend     int  // P1: 70
	RecommendationReview   int  // P1: 30
	SMTPClientDeadlineMS   Dial // P2 §6.5: UNSET; P5 lease TTL must EXCEED this (X-7 coupling).
	BrainMaxSMTPDeadlineMS Dial // mirror used by the P5 lease-TTL invariant (kept explicit).
}

// ── Policy / structural feature-flags (shape frozen, contents dial) ─────────────────
type Policy struct {
	DefaultPolicyKey string // "cold-email"
	// provider_routing, gates, order, confidence tables live in the policies store (jsonb),
	// not here — this is just which policy is the system default (P1 §4.6, single-default).
}

// Config is the whole readable configuration surface.
type Config struct {
	Env         string // "development" | "production"
	ListenAddr  string // control-plane front door bind
	DatabaseURL string // Postgres/Supabase DSN (empty => /v1/verify not-ready). Value is a secret-ish connection string, loaded from env, never committed.
	KillSwitch  KillSwitch
	ShadowModes ShadowModes
	Dials       Dials
	Policy      Policy

	// Node registry is a LIST from config (Door 5 — never a single hardcoded endpoint).
	// Phase 0 ships zero or one mock entry; the list shape is what matters.
	Nodes []NodeDescriptor
}

// CacheTTL returns the configured cache TTL as a duration, or 0 when the dial is UNSET
// (OQ-9 — no owner value). Zero means "do not cache" (the orchestrator never writes a
// guessed expires_at; §4.2 requires it NOT NULL). Value is in seconds (Dials.CacheTTLSeconds).
func (c Config) CacheTTL() time.Duration {
	if !c.Dials.CacheTTLSeconds.Set {
		return 0
	}
	return time.Duration(c.Dials.CacheTTLSeconds.Value) * time.Second
}

// ConfigFingerprint returns the P1 config fingerprint (§3.2.5): a stable SHA-256 over the
// verdict-affecting brain+policy config. In P1 there is no live engine, so the engine inputs
// are marked "unconfigured"; the fingerprint covers the policy default + the enabled cheap
// layers + the kill-switch rung. Recomputed deterministically from the config each call.
func (c Config) ConfigFingerprint() string {
	// Canonical, sorted-key serialization of the verdict-affecting inputs (§3.2.5).
	parts := []string{
		"engine=unconfigured", // P1: no live engine (SEAM → P2 adds the fork /config)
		"policy=" + c.Policy.DefaultPolicyKey,
		"layers=syntax,mx,null_mx,spam_trap,disposable,provider_classify,role,typo,free_corp",
		"rung=" + rungName(c.KillSwitch),
		"send=" + strconv.Itoa(c.Dials.RecommendationSend),
		"review=" + strconv.Itoa(c.Dials.RecommendationReview),
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return "cfg_sha256:" + hex.EncodeToString(sum[:])[:16]
}

func rungName(k KillSwitch) string {
	if k.CheapLayersOnly() {
		return "cheap-layers-only"
	}
	return "other"
}

// NodeDescriptor is one entry in the node LIST (Door 5). Tokens are referenced by id
// and resolved from secrets — never stored here in clear.
type NodeDescriptor struct {
	ID       string
	BaseURL  string
	TokenRef string // key into secrets.NodeToken(...)
	Enabled  bool
}

// Defaults returns the built-in safe configuration. Notably it places the kill-switch at
// CHEAP-LAYERS-ONLY (Rung 3) and all four mutators at recommend-only (DR-2) — the correct
// Phase-0/Phase-1 steady state. These are the manifest's documented defaults, not invented.
func Defaults() Config {
	return Config{
		Env:        "development",
		ListenAddr: ":8080",
		KillSwitch: KillSwitch{
			// No SMTP infra exists at P0/P1, and no vendor is configured — so the honest,
			// SAFE default IS cheap-layers-only. Promoting SMTP/fallback is a later, deliberate flip.
			SMTP:      SMTPDisabled,
			Fallback:  FallbackOff,
			FrontDoor: FrontDoorServing,
		},
		ShadowModes: ShadowModes{
			A1BlockRateQuarantine: ModeRecommendOnly,
			A6ConfidenceRecalib:   ModeRecommendOnly,
			A8ProviderPromotion:   ModeRecommendOnly,
			A9FallbackWeaning:     ModeRecommendOnly,
		},
		Dials: Dials{
			CacheTTLSeconds:      Dial{Set: false}, // owner OQ-9 — refuse to invent a TTL
			RawBlobRetentionDays: 90,               // P1 OQ-10 recommendation
			RecommendationSend:   70,               // P1 §7.2
			RecommendationReview: 30,               // P1 §7.2
			SMTPClientDeadlineMS: Dial{Set: false}, // P2 §6.5 owner OQ (X-7 coupling)
		},
		Policy: Policy{DefaultPolicyKey: "cold-email"},
		Nodes:  nil, // node LIST, empty at P0. Door 5: a list even when length 0/1.
	}
}

// Load builds config from defaults, then applies environment overrides. (File-based
// overrides land at P1; the precedence order is defaults -> file -> env.) It VALIDATES
// the result so a nonsensical kill-switch/mode can't start the control-plane.
func Load(getenv func(string) string) (Config, error) {
	c := Defaults()

	if v := getenv("TM_ENV"); v != "" {
		c.Env = v
	}
	if v := getenv("TM_LISTEN_ADDR"); v != "" {
		c.ListenAddr = v
	}
	if v := getenv("TM_DATABASE_URL"); v != "" {
		c.DatabaseURL = v
	}
	// Cache TTL dial (OQ-9): an operator MAY set it; absent => stays UNSET (no caching).
	if v := getenv("TM_CACHE_TTL_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return c, fmt.Errorf("config: invalid TM_CACHE_TTL_SECONDS %q", v)
		}
		c.Dials.CacheTTLSeconds = Dial{Value: n, Set: true}
	}
	// Kill-switch env overrides (operator can pull a rung without a redeploy).
	if v := getenv("TM_SMTP_MODE"); v != "" {
		c.KillSwitch.SMTP = SMTPMode(v)
	}
	if v := getenv("TM_FALLBACK_MODE"); v != "" {
		c.KillSwitch.Fallback = FallbackMode(v)
	}
	if v := getenv("TM_FRONTDOOR_MODE"); v != "" {
		c.KillSwitch.FrontDoor = FrontDoorMode(v)
	}
	// A single node from env keeps the LIST shape exercised at P0 (Door 5) without a file.
	if id := getenv("TM_NODE_ID"); id != "" {
		c.Nodes = append(c.Nodes, NodeDescriptor{
			ID:       id,
			BaseURL:  getenv("TM_NODE_BASE_URL"),
			TokenRef: id,
			Enabled:  getenv("TM_NODE_ENABLED") == "true",
		})
	}

	return c, c.Validate()
}

// LoadFromOS is the production entry point.
func LoadFromOS() (Config, error) { return Load(os.Getenv) }

// Validate rejects illegal combinations. It enforces the composability of the ladder and
// modes, not the frozen doors (those are compile-time in vocab/hash).
func (c Config) Validate() error {
	switch c.KillSwitch.SMTP {
	case SMTPEnabled, SMTPDisabled:
	default:
		return fmt.Errorf("config: invalid SMTP mode %q", c.KillSwitch.SMTP)
	}
	switch c.KillSwitch.Fallback {
	case FallbackEnabled, FallbackOff:
	default:
		return fmt.Errorf("config: invalid fallback mode %q", c.KillSwitch.Fallback)
	}
	switch c.KillSwitch.FrontDoor {
	case FrontDoorServing, FrontDoorDefer, FrontDoorReject:
	default:
		return fmt.Errorf("config: invalid front-door mode %q", c.KillSwitch.FrontDoor)
	}
	for name, m := range map[string]Mode{
		"A1": c.ShadowModes.A1BlockRateQuarantine, "A6": c.ShadowModes.A6ConfidenceRecalib,
		"A8": c.ShadowModes.A8ProviderPromotion, "A9": c.ShadowModes.A9FallbackWeaning,
	} {
		switch m {
		case ModeShadow, ModeRecommendOnly, ModeAuto:
		default:
			return fmt.Errorf("config: invalid shadow-mode for %s: %q", name, m)
		}
	}
	if c.Dials.RecommendationSend < c.Dials.RecommendationReview {
		return fmt.Errorf("config: recommendation.send (%d) must be >= review (%d)",
			c.Dials.RecommendationSend, c.Dials.RecommendationReview)
	}
	return nil
}

// Summary is a one-line, secret-free description for boot logs and the readiness probe.
func (c Config) Summary() string {
	rung := "full"
	if c.KillSwitch.CheapLayersOnly() {
		rung = "cheap-layers-only(Rung3)"
	}
	return fmt.Sprintf("env=%s listen=%s ladder=%s smtp=%s fallback=%s frontdoor=%s nodes=%d mutators=recommend-only",
		c.Env, c.ListenAddr, rung, c.KillSwitch.SMTP, c.KillSwitch.Fallback, c.KillSwitch.FrontDoor, len(c.Nodes))
}
