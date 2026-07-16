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
