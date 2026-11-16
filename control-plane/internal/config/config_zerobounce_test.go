package config

import (
	"strings"
	"testing"
	"time"
)

// The ZeroBounce dials (Docs/vendor-zerobounce.md): TM_EXTERNAL_DAILY_SPEND_CAP, TM_EXTERNAL_ZEROBOUNCE_
// KEY_ENV / TIMEOUT_MS / BASE_URL. What is pinned here: they default to a state in which nothing is sent;
// they do not themselves enable the vendor (the kill-switch rung, the DPA flag and the key still do); a bad
// value is refused at startup WITHOUT being echoed (a mistaken KEY_ENV value could be the key itself); and the
// key-env dial can never be pointed at one of our own secrets. Each refusal sits next to an accepted twin.

func zbEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestZB_Config_DefaultsSendNothing(t *testing.T) {
	c := Defaults()
	if c.Dials.ExternalDailySpendCap != 0 {
		t.Fatalf("the daily spend cap must default to 0 (= NO vendor calls), got %d", c.Dials.ExternalDailySpendCap)
	}
	if c.Dials.ExternalZeroBounceTimeoutMS != 8000 || c.ExternalZeroBounceTimeout() != 8*time.Second {
		t.Errorf("timeout default = %dms / %s, want 8000ms", c.Dials.ExternalZeroBounceTimeoutMS, c.ExternalZeroBounceTimeout())
	}
	if c.ExternalZeroBounceKeyEnvName() != "ZEROBOUNCE_API_KEY" || c.ExternalZeroBounceKeyEnv != "ZEROBOUNCE_API_KEY" {
		t.Errorf("key-env default = %q", c.ExternalZeroBounceKeyEnv)
	}
	if c.ExternalZeroBounceBase() != "https://api.zerobounce.net" {
		t.Errorf("base URL default = %q", c.ExternalZeroBounceBase())
	}
	if c.ExternalFallbackEnabled() {
		t.Fatal("the vendor ships DISABLED: the defaults must not enable it")
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("the defaults must validate: %v", err)
	}
	// Control: enabling the vendor (rung + DPA) without choosing a cap leaves the cap at 0 — still no calls.
	en, err := Load(zbEnv(map[string]string{"TM_FALLBACK_MODE": "enabled", "TM_EXTERNAL_DPA_CONFIRMED": "true"}))
	if err != nil {
		t.Fatal(err)
	}
	if !en.ExternalFallbackEnabled() || en.Dials.ExternalDailySpendCap != 0 {
		t.Errorf("enabled=%v cap=%d: enabling without a cap must leave the cap at 0", en.ExternalFallbackEnabled(), en.Dials.ExternalDailySpendCap)
	}
}

// Setting every new dial enables nothing; only the existing three switches do.
func TestZB_Config_NewDialsDoNotEnableTheVendor(t *testing.T) {
	dials := map[string]string{
		"TM_EXTERNAL_DAILY_SPEND_CAP":       "500",
		"TM_EXTERNAL_ZEROBOUNCE_KEY_ENV":    "MY_ZB_KEY",
		"TM_EXTERNAL_ZEROBOUNCE_TIMEOUT_MS": "12000",
		"TM_EXTERNAL_ZEROBOUNCE_BASE_URL":   "https://api-eu.zerobounce.net",
	}
	c, err := Load(zbEnv(dials))
	if err != nil {
		t.Fatal(err)
	}
	if c.ExternalFallbackEnabled() {
		t.Fatal("setting the vendor dials must not enable the vendor (rung + DPA do)")
	}
	if c.Dials.ExternalDailySpendCap != 500 || c.ExternalZeroBounceKeyEnvName() != "MY_ZB_KEY" ||
		c.ExternalZeroBounceTimeout() != 12*time.Second || c.ExternalZeroBounceBase() != "https://api-eu.zerobounce.net" {
		t.Errorf("the dials were not read: %+v / %q / %q", c.Dials, c.ExternalZeroBounceKeyEnv, c.ExternalZeroBounceBaseURL)
	}
	// Control: the same dials plus the rung and the DPA flag DO enable it.
	dials["TM_FALLBACK_MODE"] = "enabled"
	dials["TM_EXTERNAL_DPA_CONFIRMED"] = "true"
	c, err = Load(zbEnv(dials))
	if err != nil {
		t.Fatal(err)
	}
	if !c.ExternalFallbackEnabled() {
		t.Fatal("control: rung + DPA + dials must be egress-eligible (a key is the remaining switch)")
	}
	// And the global-fallback-kill still overrides every dial.
	dials["TM_FALLBACK_MODE"] = "off"
	c, err = Load(zbEnv(dials))
	if err != nil {
		t.Fatal(err)
	}
	if c.ExternalFallbackEnabled() {
		t.Fatal("the global-fallback-kill must override the vendor dials")
	}
}

func TestZB_Config_Load_BadValuesRefusedAndNotEchoed(t *testing.T) {
	type tc struct {
		env, val string
		ok       bool
	}
	cases := []tc{
		// spend cap: a non-negative integer; 0 is legal (= no calls)
		{"TM_EXTERNAL_DAILY_SPEND_CAP", "0", true},
		{"TM_EXTERNAL_DAILY_SPEND_CAP", "100", true},
		{"TM_EXTERNAL_DAILY_SPEND_CAP", "-1", false},
		{"TM_EXTERNAL_DAILY_SPEND_CAP", "1.5", false},
		{"TM_EXTERNAL_DAILY_SPEND_CAP", "lots", false},
		// timeout: 3000..60000 ms
		{"TM_EXTERNAL_ZEROBOUNCE_TIMEOUT_MS", "3000", true},
		{"TM_EXTERNAL_ZEROBOUNCE_TIMEOUT_MS", "60000", true},
		{"TM_EXTERNAL_ZEROBOUNCE_TIMEOUT_MS", "2999", false},
		{"TM_EXTERNAL_ZEROBOUNCE_TIMEOUT_MS", "60001", false},
		{"TM_EXTERNAL_ZEROBOUNCE_TIMEOUT_MS", "0", false},
		{"TM_EXTERNAL_ZEROBOUNCE_TIMEOUT_MS", "soon", false},
		// key env: the NAME of an upper-case environment variable, never the key, never one of ours
		{"TM_EXTERNAL_ZEROBOUNCE_KEY_ENV", "ZEROBOUNCE_API_KEY", true},
		{"TM_EXTERNAL_ZEROBOUNCE_KEY_ENV", "MY_VENDOR_KEY_2", true},
		{"TM_EXTERNAL_ZEROBOUNCE_KEY_ENV", "_X", true},
		{"TM_EXTERNAL_ZEROBOUNCE_KEY_ENV", strings.Repeat("ab", 16), false}, // a 32-character lower-case key-like value pasted by mistake
		{"TM_EXTERNAL_ZEROBOUNCE_KEY_ENV", "zerobounce_api_key", false},
		{"TM_EXTERNAL_ZEROBOUNCE_KEY_ENV", "KEY=ABC", false},
		{"TM_EXTERNAL_ZEROBOUNCE_KEY_ENV", "1BAD", false},
		{"TM_EXTERNAL_ZEROBOUNCE_KEY_ENV", "TM_PEPPER", false}, // would send the Door-3 pepper to the vendor
		{"TM_EXTERNAL_ZEROBOUNCE_KEY_ENV", "TM_SUPABASE_KEY", false},
		{"TM_EXTERNAL_ZEROBOUNCE_KEY_ENV", "TM_VENDOR_KEYS", false},
		{"TM_EXTERNAL_ZEROBOUNCE_KEY_ENV", strings.Repeat("A", 65), false},
		// base URL (TM_ENV defaults to development here)
		{"TM_EXTERNAL_ZEROBOUNCE_BASE_URL", "https://api.zerobounce.net", true},
		{"TM_EXTERNAL_ZEROBOUNCE_BASE_URL", "https://api-us.zerobounce.net", true},
		{"TM_EXTERNAL_ZEROBOUNCE_BASE_URL", "http://127.0.0.1:18081", true}, // a fixture host under development
		{"TM_EXTERNAL_ZEROBOUNCE_BASE_URL", "http://api.zerobounce.net", false},
		{"TM_EXTERNAL_ZEROBOUNCE_BASE_URL", "http://evil.example.com", false},
		{"TM_EXTERNAL_ZEROBOUNCE_BASE_URL", "https://user:s3cret@api.zerobounce.net", false},
		{"TM_EXTERNAL_ZEROBOUNCE_BASE_URL", "https://api.zerobounce.net/v2/validate", false},
		{"TM_EXTERNAL_ZEROBOUNCE_BASE_URL", "ftp://api.zerobounce.net", false},
	}
	for _, c := range cases {
		t.Run(c.env+"="+c.val, func(t *testing.T) {
			_, err := Load(zbEnv(map[string]string{c.env: c.val}))
			if (err == nil) != c.ok {
				t.Fatalf("Load(%s=%q) err=%v, want ok=%v", c.env, c.val, err, c.ok)
			}
			secretish := c.env == "TM_EXTERNAL_ZEROBOUNCE_KEY_ENV" || c.env == "TM_EXTERNAL_ZEROBOUNCE_BASE_URL" // a numeric dial may be quoted back; these two may hold a key or credentials
			if err != nil && secretish && strings.Contains(err.Error(), c.val) {
				t.Errorf("the error echoed the value (it may be the key itself): %v", err)
			}
		})
	}
}

// http:// is accepted for a loopback fixture host ONLY under TM_ENV=development or test; in production the
// same value is refused. The control is the https URL, accepted everywhere.
func TestZB_Config_BaseURL_HTTPOnlyUnderDevelopmentOrTest(t *testing.T) {
	const fixture = "http://127.0.0.1:18081"
	for _, env := range []string{"development", "test"} {
		if _, err := Load(zbEnv(map[string]string{"TM_ENV": env, "TM_EXTERNAL_ZEROBOUNCE_BASE_URL": fixture})); err != nil {
			t.Errorf("TM_ENV=%s must accept a loopback fixture over http: %v", env, err)
		}
	}
	for _, env := range []string{"production", "staging", ""} {
		m := map[string]string{"TM_ENV": env, "TM_EXTERNAL_ZEROBOUNCE_BASE_URL": fixture}
		_, err := Load(zbEnv(m))
		if env == "" { // TM_ENV unset => the default, development
			if err != nil {
				t.Errorf("an unset TM_ENV is development, which accepts the fixture: %v", err)
			}
			continue
		}
		if err == nil {
			t.Errorf("TM_ENV=%s must refuse http even for a loopback host", env)
		}
	}
	if _, err := Load(zbEnv(map[string]string{"TM_ENV": "production", "TM_EXTERNAL_ZEROBOUNCE_BASE_URL": "https://api.zerobounce.net"})); err != nil {
		t.Errorf("control: https must be accepted in production: %v", err)
	}
}

// A Config that was not built by Defaults/Load (zero values for the new fields) still validates and reads as
// the defaults; Validate also refuses a bad value set directly, so a Config built any other way cannot carry one.
func TestZB_Config_ZeroValuesAreTheDefaults_AndValidateGuards(t *testing.T) {
	c := Defaults()
	c.ExternalZeroBounceKeyEnv, c.ExternalZeroBounceBaseURL, c.Dials.ExternalZeroBounceTimeoutMS = "", "", 0
	if err := c.Validate(); err != nil {
		t.Fatalf("zero values must validate: %v", err)
	}
	if c.ExternalZeroBounceKeyEnvName() != DefaultZeroBounceKeyEnv || c.ExternalZeroBounceBase() != "https://api.zerobounce.net" || c.ExternalZeroBounceTimeout() != 8*time.Second {
		t.Errorf("zero values must read as the defaults, got %q / %q / %s", c.ExternalZeroBounceKeyEnvName(), c.ExternalZeroBounceBase(), c.ExternalZeroBounceTimeout())
	}
	bad := Defaults()
	bad.Dials.ExternalDailySpendCap = -3
	if bad.Validate() == nil {
		t.Error("a negative spend cap set directly must fail Validate")
	}
	bad = Defaults()
	bad.ExternalZeroBounceKeyEnv = "TM_PEPPER"
	if err := bad.Validate(); err == nil || strings.Contains(err.Error(), "TM_PEPPER") {
		t.Errorf("a TM_ key env must fail Validate without echoing it, got %v", err)
	}
	bad = Defaults()
	bad.Dials.ExternalZeroBounceTimeoutMS = 100
	if bad.Validate() == nil {
		t.Error("an out-of-range timeout set directly must fail Validate")
	}
}
