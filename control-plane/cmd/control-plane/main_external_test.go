package main

// Wiring tests for the ZeroBounce vendor. internal/external proves the adapter and internal/orchestrator proves the
// stage; these prove the control-plane actually BUILDS it only when every independent gate is affirmative, reads the
// key from the variable the dial NAMES, and consumes every new dial — the rate_ceiling-class failure (a dial that
// validates and then changes nothing) is exactly what a unit test of the adapter cannot see.
//
// buildExternalWith takes the environment as a function, so these tests never touch the process environment and the
// "key" they hand it is a throwaway string.

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/config"
	"github.com/coldmail-verify/control-plane/internal/external"
	"github.com/coldmail-verify/control-plane/internal/orchestrator"
	"github.com/coldmail-verify/control-plane/internal/zbtest"
)

const wiringKey = "ZBKEY-WIRING-0000-0000"

// enabledEnv is the smallest environment in which the vendor is fully switched on, plus the key.
func enabledEnv() map[string]string {
	return map[string]string{
		"TM_FALLBACK_MODE":            "enabled",
		"TM_EXTERNAL_DPA_CONFIRMED":   "true",
		"TM_EXTERNAL_DAILY_SPEND_CAP": "25",
		"ZEROBOUNCE_API_KEY":          wiringKey,
	}
}

func build(t *testing.T, env map[string]string) *orchestrator.ExternalDeps {
	t.Helper()
	get := func(k string) string { return env[k] }
	cfg, err := config.Load(get)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return buildExternalWith(cfg, get)
}

func mustZB(t *testing.T, d *orchestrator.ExternalDeps) *external.ZeroBounce {
	t.Helper()
	if d == nil {
		t.Fatal("buildExternal returned nil, want the wired stage")
	}
	zb, ok := d.Vendor.(*external.ZeroBounce)
	if !ok {
		t.Fatalf("the wired vendor is %T, want *external.ZeroBounce", d.Vendor)
	}
	return zb
}

// Every gate is independent: with all of them affirmative the stage is built, and removing any ONE returns nil
// (the layer fully off). The first row is the control every other row differs from by exactly one input.
func TestBuildExternal_EveryGateIsIndependent(t *testing.T) {
	zb := mustZB(t, build(t, enabledEnv()))
	if zb.Name() != "zerobounce" {
		t.Fatalf("vendor name = %q, want zerobounce (the budget/breaker/spend key)", zb.Name())
	}
	for name, mutate := range map[string]func(map[string]string){
		"the kill-switch rung is OFF (global-fallback-kill)": func(m map[string]string) { m["TM_FALLBACK_MODE"] = "off" },
		"the rung was never turned on":                       func(m map[string]string) { delete(m, "TM_FALLBACK_MODE") },
		"the DPA is not confirmed":                           func(m map[string]string) { delete(m, "TM_EXTERNAL_DPA_CONFIRMED") },
		"the DPA flag is not exactly \"true\"":               func(m map[string]string) { m["TM_EXTERNAL_DPA_CONFIRMED"] = "yes" },
		"the key variable is unset":                          func(m map[string]string) { delete(m, "ZEROBOUNCE_API_KEY") },
		"the key variable is blank":                          func(m map[string]string) { m["ZEROBOUNCE_API_KEY"] = "   " },
	} {
		env := enabledEnv()
		mutate(env)
		if d := build(t, env); d != nil {
			t.Errorf("%s: the stage was built — enabling must take every switch", name)
		}
	}
	// Everything at its default, key present: still off. The vendor ships disabled.
	if d := build(t, map[string]string{"ZEROBOUNCE_API_KEY": wiringKey}); d != nil {
		t.Error("a default config with a key present built the stage: the vendor must ship DISABLED")
	}
}

// The new dials are CONSUMED: the cap lands in the budget as a hard ceiling (and defaults to 0 = no calls), the
// timeout and base URL reach the client, the key is read from the variable the dial names and ONLY that one, and the
// pre-existing owner-OQ caps ride alongside unchanged.
func TestBuildExternal_NewDialsAreConsumed(t *testing.T) {
	env := enabledEnv()
	env["TM_EXTERNAL_ZEROBOUNCE_TIMEOUT_MS"] = "12000"
	env["TM_EXTERNAL_ZEROBOUNCE_BASE_URL"] = "https://api-eu.zerobounce.net"
	env["TM_EXTERNAL_ZEROBOUNCE_KEY_ENV"] = "MY_ZB_KEY"
	env["MY_ZB_KEY"] = wiringKey
	delete(env, "ZEROBOUNCE_API_KEY")
	env["TM_EXTERNAL_DAILY_CAP"] = "7" // the owner-OQ ceiling, a different dial
	d := build(t, env)
	zb := mustZB(t, d)
	if zb.Timeout() != 12*time.Second {
		t.Errorf("timeout = %s, want 12s (TM_EXTERNAL_ZEROBOUNCE_TIMEOUT_MS)", zb.Timeout())
	}
	if zb.Endpoint() != "https://api-eu.zerobounce.net/v2/validate" {
		t.Errorf("endpoint = %q (TM_EXTERNAL_ZEROBOUNCE_BASE_URL)", zb.Endpoint())
	}
	if !d.Budget.VendorDailyCapEnforced || d.Budget.VendorDailyCap != 25 || d.Budget.DailyCap != 7 {
		t.Errorf("budget = %+v, want the hard vendor cap 25 enforced alongside the owner-OQ DailyCap 7", d.Budget)
	}
	if d.GloballyDisabled {
		t.Error("a wired stage must not be globally disabled")
	}

	// The key variable is the one the DIAL names: the default variable alone is not enough...
	only := enabledEnv()
	only["TM_EXTERNAL_ZEROBOUNCE_KEY_ENV"] = "MY_ZB_KEY"
	if build(t, only) != nil {
		t.Error("the stage was built from ZEROBOUNCE_API_KEY although the dial names MY_ZB_KEY")
	}
	// ...and the named one is enough without the default.
	named := enabledEnv()
	delete(named, "ZEROBOUNCE_API_KEY")
	named["TM_EXTERNAL_ZEROBOUNCE_KEY_ENV"] = "MY_ZB_KEY"
	named["MY_ZB_KEY"] = wiringKey
	if build(t, named) == nil {
		t.Error("control: the stage must be built from the variable the dial names")
	}
}

// The cap defaults to 0 and 0 is a CLOSED door: enabling the vendor without choosing a cap wires it but Decide
// refuses every call. The control is the same wiring with a cap.
func TestBuildExternal_CapDefaultsToZero_AndRefusesEveryCall(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	env := enabledEnv()
	delete(env, "TM_EXTERNAL_DAILY_SPEND_CAP")
	d := build(t, env)
	mustZB(t, d)
	if dec := external.Decide(d.Budget, d.Breaker, external.SpendFacts{}, now); dec.Allow {
		t.Fatalf("a vendor wired with the default cap allowed a call: %+v", dec)
	}
	env["TM_EXTERNAL_DAILY_SPEND_CAP"] = "5"
	d = build(t, env)
	if dec := external.Decide(d.Budget, d.Breaker, external.SpendFacts{SpentDay: 4}, now); !dec.Allow {
		t.Errorf("control: with a cap of 5 and 4 spent the next call must be allowed, got %+v", dec)
	}
	if dec := external.Decide(d.Budget, d.Breaker, external.SpendFacts{SpentDay: 5}, now); dec.Allow {
		t.Errorf("control: with a cap of 5 and 5 spent the next call must be refused, got %+v", dec)
	}
}

// The wired client really sends the key held in the variable the dial names (the stand-in records only its
// LENGTH) to the base URL the dial names — end to end through config.Load and buildExternal, with a fixture host
// accepted over http only because TM_ENV is development.
func TestBuildExternal_ReadsTheNamedKeyAndTheBaseURL(t *testing.T) {
	base := zbtest.URL(t)
	long := strings.Repeat("k", 40)
	env := enabledEnv()
	env["TM_EXTERNAL_ZEROBOUNCE_BASE_URL"] = base
	env["TM_EXTERNAL_ZEROBOUNCE_KEY_ENV"] = "MY_ZB_KEY"
	env["MY_ZB_KEY"] = long
	env["ZEROBOUNCE_API_KEY"] = "short-decoy" // a different length: if this is read instead, the check below fails
	zb := mustZB(t, build(t, env))
	tag := zbtest.Tag(t)
	if _, err := zb.Verify(context.Background(), zbtest.Addr("valid", tag, "gmail.com"), "idem-"+tag); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	reqs := zbtest.Requests(t, tag)
	if len(reqs) != 1 || reqs[0].KeyLen != 40 {
		t.Fatalf("requests = %+v, want one carrying a 40-byte key (the variable the dial names)", reqs)
	}
	// Paired control: with TM_ENV=production the same http fixture URL is refused at config load.
	env["TM_ENV"] = "production"
	if _, err := config.Load(func(k string) string { return env[k] }); err == nil {
		t.Error("control: production must refuse an http base URL")
	}
}

// Nothing the build logs contains the key; the failure path names the VARIABLE and never a value; and the stale
// policy.vendor field is surfaced as informational rather than silently ignored.
func TestBuildExternal_LogsNameTheVariableNeverTheKey(t *testing.T) {
	orig := log.Writer()
	t.Cleanup(func() { log.SetOutput(orig) })

	var ok bytes.Buffer
	log.SetOutput(&ok)
	env := enabledEnv()
	env["TM_EXTERNAL_ZEROBOUNCE_KEY_ENV"] = "MY_ZB_KEY"
	env["MY_ZB_KEY"] = wiringKey
	mustZB(t, build(t, env))
	if strings.Contains(ok.String(), wiringKey) {
		t.Errorf("the key reached the log: %s", ok.String())
	}
	if !strings.Contains(ok.String(), "informational") || !strings.Contains(ok.String(), "zerobounce") {
		t.Errorf("the log must surface that policy.vendor is informational and the compiled vendor is zerobounce: %s", ok.String())
	}

	var missing bytes.Buffer
	log.SetOutput(&missing)
	env2 := enabledEnv()
	env2["TM_EXTERNAL_ZEROBOUNCE_KEY_ENV"] = "MY_ZB_KEY"
	delete(env2, "MY_ZB_KEY")
	if build(t, env2) != nil {
		t.Fatal("a missing key must leave the stage off")
	}
	if !strings.Contains(missing.String(), "MY_ZB_KEY") {
		t.Errorf("the not-ready log must NAME the variable so an operator can fix it: %s", missing.String())
	}
	if strings.Contains(missing.String(), wiringKey) {
		t.Errorf("the key reached the log: %s", missing.String())
	}
}
