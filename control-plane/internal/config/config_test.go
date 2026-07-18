package config

import "testing"

// These tests pin two Phase-0 done-criteria: (1) the kill-switch defaults to
// cheap-layers-only — the only rung reachable without SMTP; (2) the four day-one
// mutators default to recommend-only (DR-2). A regression on either fails here.

func TestDefaultsAreCheapLayersOnly(t *testing.T) {
	c := Defaults()
	if !c.KillSwitch.CheapLayersOnly() {
		t.Fatalf("Phase-0/1 steady state must be cheap-layers-only (Rung 3); got smtp=%s fallback=%s",
			c.KillSwitch.SMTP, c.KillSwitch.Fallback)
	}
}

func TestDefaultMutatorsAreRecommendOnly(t *testing.T) {
	m := Defaults().ShadowModes
	for name, got := range map[string]Mode{
		"A1": m.A1BlockRateQuarantine, "A6": m.A6ConfidenceRecalib,
		"A8": m.A8ProviderPromotion, "A9": m.A9FallbackWeaning,
	} {
		if got != ModeRecommendOnly {
			t.Errorf("mutator %s default = %q, want recommend_only (DR-2)", name, got)
		}
	}
}

func TestUnsetDialIsNotZero(t *testing.T) {
	// An owner-OQ dial must read UNSET, not silently substitute 0 (config-manifest §8.2).
	c := Defaults()
	if c.Dials.CacheTTLSeconds.Set {
		t.Error("cache TTL is an owner OQ (OQ-9) and must default UNSET, not a guessed number")
	}
	if c.Dials.CacheTTLSeconds.String() != "UNSET(owner-OQ)" {
		t.Errorf("unset dial String()=%q", c.Dials.CacheTTLSeconds.String())
	}
}

func TestValidateRejectsBadModes(t *testing.T) {
	c := Defaults()
	c.KillSwitch.SMTP = "bogus"
	if err := c.Validate(); err == nil {
		t.Error("Validate must reject an invalid SMTP mode")
	}
	c = Defaults()
	c.Dials.RecommendationSend = 10
	c.Dials.RecommendationReview = 30
	if err := c.Validate(); err == nil {
		t.Error("Validate must reject send-threshold < review-threshold")
	}
}

func TestLoadAppliesEnvOverrides(t *testing.T) {
	env := map[string]string{
		"TM_ENV": "production", "TM_SMTP_MODE": "enabled",
		"TM_NODE_ID": "mock-1", "TM_NODE_BASE_URL": "http://localhost:3000", "TM_NODE_ENABLED": "false",
	}
	c, err := Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Env != "production" || c.KillSwitch.SMTP != SMTPEnabled {
		t.Errorf("env overrides not applied: env=%s smtp=%s", c.Env, c.KillSwitch.SMTP)
	}
	if len(c.Nodes) != 1 || c.Nodes[0].ID != "mock-1" {
		t.Errorf("node LIST from env not applied: %+v", c.Nodes)
	}
}

// The GAP-31 proof AT THE CONFIG LAYER: adding node #2 is a CONFIG edit (a numbered env block), not a
// code change. The same Load path produces a 2-node list; nothing in the brain changes.
func TestLoad_NodeTwoIsConfigOnly_GAP31(t *testing.T) {
	env := map[string]string{
		"TM_SMTP_MODE": "enabled",
		"TM_NODE_ID":   "n1", "TM_NODE_BASE_URL": "http://box1", "TM_NODE_ENABLED": "true", "TM_NODE_PUBLIC_EGRESS_IP": "203.0.113.10",
		// ↓↓↓ node #2 — a config-only addition (a numbered env block), zero brain code change ↓↓↓
		"TM_NODE_2_ID": "n2", "TM_NODE_2_BASE_URL": "http://box2", "TM_NODE_2_ENABLED": "true", "TM_NODE_2_PUBLIC_EGRESS_IP": "203.0.113.11",
	}
	c, err := Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Nodes) != 2 {
		t.Fatalf("adding node #2 must be a +1 config-list entry, got %d nodes", len(c.Nodes))
	}
	if c.Nodes[1].ID != "n2" || c.Nodes[1].BaseURL != "http://box2" || c.Nodes[1].PublicEgressIP != "203.0.113.11" {
		t.Fatalf("node #2 descriptor not parsed from numbered env: %+v", c.Nodes[1])
	}
	// The numbered scan stops at the first gap (no TM_NODE_3_*), so exactly 2 nodes.
}

func TestLoad_P4WatcherDialsDefaultAndOverride(t *testing.T) {
	// Defaults present. CanaryWarmupCap (20) must be >= CanaryMinSamples (10) — the round-2 review
	// finding: a smaller cap than the sample gate makes a recovering node stuck-forever (GAP-30).
	def := Defaults()
	if def.Dials.FleetWidePct != 50 || def.Dials.BlockMinSamples != 20 || def.Dials.CanaryWarmupCap != 20 {
		t.Fatalf("P4 watcher defaults not set: %+v", def.Dials)
	}
	if def.Dials.CanaryWarmupCap < def.Dials.CanaryMinSamples {
		t.Fatalf("canary cap (%d) must be >= min samples (%d) or a canary can never promote (GAP-30 stuck-forever)", def.Dials.CanaryWarmupCap, def.Dials.CanaryMinSamples)
	}
	if def.WatcherCadence() != 45*1e9 {
		t.Fatalf("watcher cadence default want 45s, got %v", def.WatcherCadence())
	}
	// Overrides applied.
	env := map[string]string{
		"TM_WATCHER_CADENCE_SECONDS": "30", "TM_BLOCK_MIN_SAMPLES": "40",
		"TM_FLEET_WIDE_PCT": "60", "TM_BLOCK_QUAR_THRESHOLD_PCT": "25",
	}
	c, err := Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Dials.WatcherCadenceSeconds != 30 || c.Dials.BlockMinSamples != 40 || c.Dials.FleetWidePct != 60 || c.Dials.BlockQuarThresholdPct != 25 {
		t.Fatalf("P4 dial overrides not applied: %+v", c.Dials)
	}
}

// Round-2 review finding D (GAP-28): TM_FLEET_WIDE_PCT ≥ 100 makes the fraction-brake mathematically
// unable to fire — it must be rejected at startup, never silently disable the brake.
func TestLoad_FleetWidePctUpperBound_FindingD(t *testing.T) {
	for _, v := range []string{"100", "150", "250"} {
		if _, err := Load(func(k string) string {
			if k == "TM_FLEET_WIDE_PCT" {
				return v
			}
			return ""
		}); err == nil {
			t.Fatalf("TM_FLEET_WIDE_PCT=%s must be rejected (≥100 disables the GAP-28 brake), got nil error", v)
		}
	}
	// A sane majority is accepted.
	if _, err := Load(func(k string) string {
		if k == "TM_FLEET_WIDE_PCT" {
			return "60"
		}
		return ""
	}); err != nil {
		t.Fatalf("TM_FLEET_WIDE_PCT=60 must be accepted, got %v", err)
	}
	// The default (50) validates.
	if err := Defaults().Validate(); err != nil {
		t.Fatalf("the default config must validate, got %v", err)
	}
}

// A cadence of 0 is a valid override (one-shot, no loop); a negative/invalid dial is rejected.
func TestLoad_P4DialValidation(t *testing.T) {
	if _, err := Load(func(k string) string {
		if k == "TM_WATCHER_CADENCE_SECONDS" {
			return "0"
		}
		return ""
	}); err != nil {
		t.Fatalf("cadence 0 (one-shot) must be allowed, got %v", err)
	}
	if _, err := Load(func(k string) string {
		if k == "TM_BLOCK_MIN_SAMPLES" {
			return "0" // 0 is NOT allowed for a threshold dial (zero-disallowed)
		}
		return ""
	}); err == nil {
		t.Fatal("TM_BLOCK_MIN_SAMPLES=0 must be rejected (a zero sample gate is unsafe)")
	}
	// A canary cap BELOW the min-samples gate must be REJECTED (GAP-30 stuck-forever guard).
	if _, err := Load(func(k string) string {
		switch k {
		case "TM_CANARY_WARMUP_CAP":
			return "5"
		case "TM_CANARY_MIN_SAMPLES":
			return "10"
		}
		return ""
	}); err == nil {
		t.Fatal("canary_warmup_cap(5) < canary_min_samples(10) must be rejected (a recovering node could never promote)")
	}
}

// TestLoad_P5QueueDialsDefaultAndOverride proves the P5 queue dials were promoted from buildQueue
// code-defaults to config (PROJECT_RULES §6): defaults carry the §4.2/§4.5 spec values, env
// overrides apply, and the §7.5 submission-backpressure dials are owner-OQ → read UNSET (never a
// guessed number).
func TestLoad_P5QueueDialsDefaultAndOverride(t *testing.T) {
	def := Defaults()
	// §4.2/§4.5 dials carry the spec defaults.
	if def.Dials.QueueBackoffBaseSeconds != 300 || def.Dials.QueueBackoffFactor != 2 ||
		def.Dials.QueueBackoffCapSeconds != 3600 || def.Dials.QueueBackoffJitterPct != 15 ||
		def.Dials.QueueMaxAttempts != 4 || def.Dials.QueueMaxRetryAgeSeconds != 21600 ||
		def.Dials.QueueLeaseMarginSeconds != 120 {
		t.Fatalf("P5 queue dial defaults not set: %+v", def.Dials)
	}
	// The §7.5 bulk-max dial is owner-OQ → UNSET (never guessed). (The other two backpressure dials —
	// max-in-flight-jobs, per-job-% — are deliberately NOT wired as config: no enforcement path yet,
	// and a settable-but-ignored knob is a silent footgun — the P5→P6 review finding.)
	if def.Dials.QueueBulkMaxAddresses.Set {
		t.Fatal("P5 bulk-max dial must default UNSET (owner OQ), never a guessed number")
	}
	if def.Dials.QueueBulkMaxAddresses.String() != "UNSET(owner-OQ)" {
		t.Fatalf("an UNSET dial must read UNSET, got %q", def.Dials.QueueBulkMaxAddresses.String())
	}
	// The typed lease-TTL accessor: TTL = maxSMTPdeadline + margin (strictly exceeds the deadline).
	if got := def.QueueLeaseTTL(60 * 1e9); got != (60+120)*1e9 {
		t.Fatalf("lease TTL want 180s, got %v", got)
	}
	// Overrides apply (int dials + a Set backpressure dial).
	env := map[string]string{
		"TM_QUEUE_MAX_ATTEMPTS": "6", "TM_QUEUE_BACKOFF_BASE_SECONDS": "600",
		"TM_QUEUE_BULK_MAX_ADDRESSES": "5000",
	}
	c, err := Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Dials.QueueMaxAttempts != 6 || c.Dials.QueueBackoffBaseSeconds != 600 {
		t.Fatalf("P5 int-dial overrides not applied: %+v", c.Dials)
	}
	if !c.Dials.QueueBulkMaxAddresses.Set || c.Dials.QueueBulkMaxAddresses.Value != 5000 {
		t.Fatalf("TM_QUEUE_BULK_MAX_ADDRESSES override not applied: %+v", c.Dials.QueueBulkMaxAddresses)
	}
}

// TestLoad_P5LeaseMarginGuard is the GAP-37 config-layer guard: the lease margin MUST be > 0 so the
// lease TTL strictly exceeds the max SMTP deadline (else a slow attempt loses its lease → double-run).
func TestLoad_P5LeaseMarginGuard(t *testing.T) {
	if _, err := Load(func(k string) string {
		if k == "TM_QUEUE_LEASE_MARGIN_SECONDS" {
			return "0"
		}
		return ""
	}); err == nil {
		t.Fatal("queue_lease_margin_seconds=0 must be rejected (GAP-37: lease TTL must exceed the SMTP deadline)")
	}
	// A positive margin validates.
	if _, err := Load(func(k string) string {
		if k == "TM_QUEUE_LEASE_MARGIN_SECONDS" {
			return "90"
		}
		return ""
	}); err != nil {
		t.Fatalf("a positive lease margin must be accepted, got %v", err)
	}
}
