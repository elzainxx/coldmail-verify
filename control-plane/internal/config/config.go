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

	// Phase 3 (§4.5/§5.2) — the audit/warmup DIALs (volumes are dials; existence is locked).
	AuditIntervalSeconds int // §4.5 scheduled re-audit cadence; recommend hourly (3600) for a first box.
	AuditMaxAgeSeconds   int // §4.5 staleness window; recommend 2× the interval. A green older than this → not fresh → excluded.
	AuditRecoverGreens   int // §4.5 OQ-P3-7: consecutive greens to auto-recover from quarantine (asymmetric; quarantine on first red).
	WarmupDailyCap       int // §5.2 per-node/per-day :smtp verification ceiling; 0 => warmup NOT enforced (a mature node). Counts verifications (catch-all ≈ 2 connections — GAP-13/§5.2).

	// Phase 4 (§5) — the health-watcher control-loop DIALs (volumes are dials; existence is locked).
	// The watcher REPLACES the P3 minimal re-audit ticker: it owns the audit cadence + adds liveness
	// + block-rate. Start conservative (§5.4 "longer window = less twitchy"). OQ-P4-13 recommends
	// T=30–60s, block-rate window 15–30m. The block-rate quarantine is recommend-only by default
	// (DR-2, ShadowModes.A1BlockRateQuarantine) so the watcher performs ZERO live block-rate mutation
	// until an owner promotes A1 to auto (the fresh-system invariant).
	WatcherCadenceSeconds  int // §5.1 T — control-loop cadence (0 => one pass at boot, no loop)
	BlockRateWindowSeconds int // §5.2 rolling block-rate window
	BlockMinSamples        int // §5.3/GAP-27 — block-rate trigger inert below this attempt count
	BlockQuarThresholdPct  int // §5.4 — block-rate ≥ this % (with samples, pool healthy) → quarantine-eligible
	BlockRecoverThreshPct  int // §5.4 — canary block-rate ≤ this % (sustained) → recover (asymmetric, lower)
	CanaryRecoverGreens    int // §5.4 — consecutive clean canary cycles before recovering→active
	CanaryMinSamples       int // §5.4/GAP-30 — MIN real canary attempts before a block-rate recovery may promote (no zero-sample promotion — the Phase-4 review finding)
	LivenessMisses         int // §5.4 — M consecutive liveness failures → quarantine (box down/wedged)
	FleetWidePct           int // §5.5/GAP-28 — if this % of ACTIVE nodes trip at once → BRAKE (alert, don't quarantine)
	CanaryWarmupCap        int // §5.4/PM3 — the low warmup cap a recovered node re-warms from
	PoolEligibleFloor      int // §8.2 — alert when the eligible set ≤ this floor (recommend 1)
	RateSaturationCount    int // §8.2 — alert when rate_limited degrades in the window ≥ this
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

// SMTPClientDeadline returns the brain-side SMTP client deadline (§6.5), or 0 when the dial is
// UNSET (the node client falls back to its own safe default). This is the brain's own budget on a
// /v1/verify call so a hung box can't hang the brain; a timeout maps to smtp_timeout/unknown.
func (c Config) SMTPClientDeadline() time.Duration {
	if !c.Dials.SMTPClientDeadlineMS.Set {
		return 0
	}
	return time.Duration(c.Dials.SMTPClientDeadlineMS.Value) * time.Millisecond
}

// SMTPEnabled reports whether the SMTP path may run (kill-switch Rung 1 not pulled). Even when
// true, SMTP only runs if a node is actually routable — otherwise the brain degrades to the cheap
// verdict (never invents SMTP).
func (c Config) SMTPEnabled() bool { return c.KillSwitch.SMTP == SMTPEnabled }

// AuditInterval / AuditMaxAge / WarmupDailyCap / AuditRecoverGreens are the Phase-3 §4.5/§5.2 DIALs
// as typed accessors (the scheduler + the router filter read these).
func (c Config) AuditInterval() time.Duration {
	return time.Duration(c.Dials.AuditIntervalSeconds) * time.Second
}
func (c Config) AuditMaxAge() time.Duration {
	return time.Duration(c.Dials.AuditMaxAgeSeconds) * time.Second
}

// WatcherCadence / BlockRateWindow are the Phase-4 §5 health-watcher DIALs as typed durations.
func (c Config) WatcherCadence() time.Duration {
	return time.Duration(c.Dials.WatcherCadenceSeconds) * time.Second
}
func (c Config) BlockRateWindow() time.Duration {
	return time.Duration(c.Dials.BlockRateWindowSeconds) * time.Second
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

// EngineConfigFingerprint composes the P2 config fingerprint (§4.6, GAP-3): the same canonical
// brain+policy inputs as ConfigFingerprint, but with the engine marker REPLACED by the engine's
// own canonical fingerprint (fetched from the fork's /v1/config, computed brain-side). This
// retires the P1 "engine=unconfigured" placeholder — from P2 the fingerprint covers the real
// engine config, so a config-affecting engine change yields a new fingerprint that naturally
// invalidates the (email_hash, config_fingerprint) cache. engineFP is the brain-computed hash.
func (c Config) EngineConfigFingerprint(engineFP string) string {
	parts := []string{
		"engine=" + engineFP, // was "engine=unconfigured" in P1 (§4.6 resolves GAP-3)
		"policy=" + c.Policy.DefaultPolicyKey,
		"layers=syntax,mx,null_mx,spam_trap,disposable,provider_classify,role,typo,free_corp,smtp",
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

	// PublicEgressIP is the node's KNOWN public egress IP — the IP its outbound SMTP actually
	// originates from and that PTR resolves to (P3 §5.4 OQ-P3-11: the brain registry is authoritative
	// for verifier_ip; the box never self-reports its reputation-bearing identity). It is the GAP-24
	// cross-check anchor (§6.6.1): the router refuses :smtp unless the audit's self-detected
	// current_host_ip EQUALS this value, so a multi-homed box can't pass green on its private IP.
	// PENDING-REAL-BOX: the VALUE is set once a real host is provisioned; empty here (local build) →
	// the GAP-24 conjunct fails safe (no :smtp on a green whose IP we can't confirm).
	PublicEgressIP string
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
			// Phase-3 audit/warmup dials (§4.5/§5.2). Conservative defaults (OQ-P3-7/OQ-P3-10):
			AuditIntervalSeconds: 3600, // hourly re-audit for a first box (§4.5)
			AuditMaxAgeSeconds:   7200, // 2× the interval → a stale green is excluded (§4.5)
			AuditRecoverGreens:   3,    // 3 consecutive greens to auto-recover (§4.5 asymmetric)
			WarmupDailyCap:       50,   // §5.2 conservative day-1 start (~50/day, OQ-P3-10 dial)
			// Phase-4 health-watcher dials (§5). Conservative defaults (OQ-P4-13; §5.4 asymmetric):
			WatcherCadenceSeconds:  45,   // §5.1 T (30–60s recommended)
			BlockRateWindowSeconds: 1800, // §5.2 30-minute rolling window (start conservative)
			BlockMinSamples:        20,   // §5.3/GAP-27 sample gate
			BlockQuarThresholdPct:  30,   // §5.4 quarantine at ≥30% block-rate
			BlockRecoverThreshPct:  5,    // §5.4 recover at ≤5% (asymmetric, lower)
			CanaryRecoverGreens:    3,    // §5.4 consecutive clean canary cycles
			CanaryMinSamples:       10,   // §5.4/GAP-30 real canary attempts before promotion (no zero-sample)
			LivenessMisses:         3,    // §5.4 M consecutive liveness misses
			FleetWidePct:           50,   // §5.5/GAP-28 majority of active nodes = systemic → brake
			// CanaryWarmupCap is the per-window meter for a recovering node's canary trickle. It MUST be
			// >= CanaryMinSamples, or the canary can never accrue enough real attempts to promote —
			// re-introducing the GAP-30 "stuck forever" one-way door (the Phase-4 round-2 review found
			// this exact inconsistency when the cap was below the gate). Validate() enforces it; the
			// default sits comfortably above CanaryMinSamples so a recovering node can actually recover.
			CanaryWarmupCap:     20, // §5.4/PM3 low re-warm start — but >= CanaryMinSamples so the canary can promote
			PoolEligibleFloor:   1,  // §8.2 alert one-quarantine-from-all-degrade
			RateSaturationCount: 50, // §8.2 sustained rate_limited degrades in the window
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
	// SMTP client deadline dial (P2 §6.5): the brain's own budget on a /v1/verify call.
	if v := getenv("TM_SMTP_CLIENT_DEADLINE_MS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return c, fmt.Errorf("config: invalid TM_SMTP_CLIENT_DEADLINE_MS %q", v)
		}
		c.Dials.SMTPClientDeadlineMS = Dial{Value: n, Set: true}
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
	// The node LIST from env (Door 5). Node #1 uses the bare TM_NODE_* keys (the P0/P3 shape); nodes
	// #2..#N use numbered keys TM_NODE_<i>_* (i≥2). This is how P4 proves "add node #2 = +1 config
	// line" (§7/GAP-31): standing up a second node is a CONFIG edit — a numbered env block — with ZERO
	// brain code change. The router/watcher/limiter are all list-shaped/per-node (built that way since
	// P1), so the same code serves N=1 and N=2. (A real #2 also needs the §6 provisioning — the
	// honest bound of GAP-31: the brain is N-agnostic; the world still makes you earn each clean IP.)
	if id := getenv("TM_NODE_ID"); id != "" {
		c.Nodes = append(c.Nodes, NodeDescriptor{
			ID:       id,
			BaseURL:  getenv("TM_NODE_BASE_URL"),
			TokenRef: id,
			Enabled:  getenv("TM_NODE_ENABLED") == "true",
			// P3 §5.4/GAP-24: the node's KNOWN public egress IP (== the PTR'd/audited IP). PENDING-
			// REAL-BOX — set once a real host is provisioned; empty in the local build → the GAP-24
			// conjunct fails safe (no :smtp on a green whose IP the brain can't confirm).
			PublicEgressIP: getenv("TM_NODE_PUBLIC_EGRESS_IP"),
		})
	}
	// Nodes #2..#N — numbered, config-only (the GAP-31 proof). Stops at the first gap in the sequence.
	for i := 2; ; i++ {
		p := "TM_NODE_" + strconv.Itoa(i) + "_"
		id := getenv(p + "ID")
		if id == "" {
			break
		}
		c.Nodes = append(c.Nodes, NodeDescriptor{
			ID:             id,
			BaseURL:        getenv(p + "BASE_URL"),
			TokenRef:       id,
			Enabled:        getenv(p+"ENABLED") == "true",
			PublicEgressIP: getenv(p + "PUBLIC_EGRESS_IP"),
		})
	}

	// Phase-3 audit/warmup dial overrides (§4.5/§5.2). Each is optional; absent => the safe default.
	if v := getenv("TM_AUDIT_INTERVAL_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return c, fmt.Errorf("config: invalid TM_AUDIT_INTERVAL_SECONDS %q", v)
		}
		c.Dials.AuditIntervalSeconds = n
	}
	if v := getenv("TM_AUDIT_MAX_AGE_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return c, fmt.Errorf("config: invalid TM_AUDIT_MAX_AGE_SECONDS %q", v)
		}
		c.Dials.AuditMaxAgeSeconds = n
	}
	if v := getenv("TM_AUDIT_RECOVER_GREENS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return c, fmt.Errorf("config: invalid TM_AUDIT_RECOVER_GREENS %q (must be >= 1)", v)
		}
		c.Dials.AuditRecoverGreens = n
	}
	if v := getenv("TM_WARMUP_DAILY_CAP"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return c, fmt.Errorf("config: invalid TM_WARMUP_DAILY_CAP %q", v)
		}
		c.Dials.WarmupDailyCap = n
	}

	// Phase-4 health-watcher dial overrides (§5). Each optional; absent => the safe default. All are
	// non-negative (a 0 cadence disables the loop; 0 fleet-pct is clamped to the safe default in the
	// health package's withDefaults, so the brake is never accidentally disabled — GAP-28).
	for _, d := range []struct {
		env  string
		dst  *int
		zero bool // true => allow 0 (e.g. cadence 0 = one-shot)
	}{
		{"TM_WATCHER_CADENCE_SECONDS", &c.Dials.WatcherCadenceSeconds, true},
		{"TM_BLOCK_RATE_WINDOW_SECONDS", &c.Dials.BlockRateWindowSeconds, false},
		{"TM_BLOCK_MIN_SAMPLES", &c.Dials.BlockMinSamples, false},
		{"TM_BLOCK_QUAR_THRESHOLD_PCT", &c.Dials.BlockQuarThresholdPct, false},
		{"TM_BLOCK_RECOVER_THRESHOLD_PCT", &c.Dials.BlockRecoverThreshPct, false},
		{"TM_CANARY_RECOVER_GREENS", &c.Dials.CanaryRecoverGreens, false},
		{"TM_CANARY_MIN_SAMPLES", &c.Dials.CanaryMinSamples, false},
		{"TM_LIVENESS_MISSES", &c.Dials.LivenessMisses, false},
		{"TM_FLEET_WIDE_PCT", &c.Dials.FleetWidePct, false},
		{"TM_CANARY_WARMUP_CAP", &c.Dials.CanaryWarmupCap, false},
		{"TM_POOL_ELIGIBLE_FLOOR", &c.Dials.PoolEligibleFloor, true},
		{"TM_RATE_SATURATION_COUNT", &c.Dials.RateSaturationCount, true},
	} {
		if v := getenv(d.env); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 || (!d.zero && n == 0) {
				return c, fmt.Errorf("config: invalid %s %q", d.env, v)
			}
			*d.dst = n
		}
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
	// §5.4/GAP-30: the canary meter must allow enough real attempts to satisfy the promotion sample
	// gate, or a recovering node can NEVER accrue MinCanarySamples → stuck-forever (the round-2 review
	// finding). CanaryWarmupCap>0 (a canary is always metered) AND >= CanaryMinSamples. Both dials are
	// per the block-rate window in practice; require the meter to cover the gate with margin=equality.
	if c.Dials.CanaryWarmupCap > 0 && c.Dials.CanaryMinSamples > c.Dials.CanaryWarmupCap {
		return fmt.Errorf("config: canary_warmup_cap (%d) must be >= canary_min_samples (%d) or a recovering node can never accrue enough samples to promote (GAP-30 stuck-forever)",
			c.Dials.CanaryWarmupCap, c.Dials.CanaryMinSamples)
	}
	// §5.5/GAP-28 (round-2 review finding D): the fleet-wide brake fraction is FleetWidePct/100, and
	// the systemic-spike test is `elevated >= fraction × active`. A percentage ≥ 100 yields a fraction
	// ≥ 1.0, so the test needs EVERY active node elevated in one cycle — with staggered onset that
	// essentially never happens, silently DISABLING "the single most important safety line" with no
	// startup error. The loader already rejects 0/negative; here we bound the top. Require 1..99 so the
	// brake can always fire on a genuine majority. (The active-floor backstop still bounds the worst
	// case, but a dead fraction-brake + no fleet_wide alert is exactly the auto-heal→auto-outage hole.)
	if c.Dials.FleetWidePct < 1 || c.Dials.FleetWidePct > 99 {
		return fmt.Errorf("config: fleet_wide_pct (%d) must be in 1..99 — a value ≥100 makes the fleet-wide cascade brake (GAP-28) mathematically unable to fire, silently disabling it",
			c.Dials.FleetWidePct)
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
