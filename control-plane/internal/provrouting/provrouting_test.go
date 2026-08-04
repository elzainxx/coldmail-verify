package provrouting

import (
	"encoding/json"
	"testing"

	"github.com/coldmail-verify/control-plane/internal/policy"
)

// The render is generated from the ACTUAL cold-email policy provider_routing (the single source),
// so a policy change flows to the engine backstop automatically (§5.1/PM1: one editable source).
func policyProviderRouting(t *testing.T) map[string]string {
	t.Helper()
	def, err := policy.ColdEmailDefinition()
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	return def.ProviderRouting
}

func TestRender_FromPolicySource(t *testing.T) {
	pr := policyProviderRouting(t)
	got := Render(pr)
	// The policy routes google/yahoo/o365 → mx; each provider's consumer domains must all be :mx.
	for _, d := range []string{"gmail.com", "googlemail.com", "yahoo.com", "outlook.com", "hotmail.com", "live.com"} {
		if got[d] != "mx" {
			t.Fatalf("expected %s -> mx in the rendered engine map, got %q", d, got[d])
		}
	}
	// An honest corporate domain is NOT in the map (default_validation_type=smtp applies, §5.1).
	if _, ok := got["acmecorp.com"]; ok {
		t.Fatal("honest corporate domains must NOT be in validation_type_for (they route :smtp)")
	}
}

func TestDrift_InSyncIsClean(t *testing.T) {
	pr := policyProviderRouting(t)
	expected := Render(pr)
	// The box reports EXACTLY the render → no drift.
	if d := Drift(expected, expected); len(d) != 0 {
		t.Fatalf("an in-sync box must report no drift, got %v", d)
	}
}

func TestDrift_MissingMappingFlagged(t *testing.T) {
	expected := Render(policyProviderRouting(t))
	reported := map[string]string{} // the box has NO validation_type_for (dropped gmail → :mx)
	d := Drift(expected, reported)
	if len(d) == 0 {
		t.Fatal("a box missing the provider :mx mappings must be a drift alarm (GAP-19)")
	}
}

func TestDrift_TypeMismatchFlagged(t *testing.T) {
	expected := Render(policyProviderRouting(t))
	reported := Render(policyProviderRouting(t))
	reported["gmail.com"] = "smtp" // the box would :smtp gmail — a dangerous drift (SVG ⑤)
	d := Drift(expected, reported)
	found := false
	for _, s := range d {
		if s == "type mismatch: gmail.com brain=mx box=smtp" {
			found = true
		}
	}
	if !found {
		t.Fatalf("gmail :smtp on the box must be flagged as a type-mismatch drift, got %v", d)
	}
}

func TestDrift_UnexpectedManagedOverrideFlagged(t *testing.T) {
	// The brain routes only google to :mx, but the box also routes yahoo (a managed provider domain)
	// — the brain would send yahoo for SMTP thinking the box agrees. Flag it.
	expected := Render(map[string]string{"google": "mx"})
	reported := Render(map[string]string{"google": "mx", "yahoo": "mx"})
	d := Drift(expected, reported)
	if len(d) == 0 {
		t.Fatalf("a box override on a managed provider domain the brain didn't render must be flagged, got %v", d)
	}
}

func TestDrift_CaseInsensitiveDomains(t *testing.T) {
	expected := Render(policyProviderRouting(t))
	reported := map[string]string{}
	for k, v := range expected {
		reported[upperFirst(k)] = v // box reports mixed-case domain keys
	}
	if d := Drift(expected, reported); len(d) != 0 {
		t.Fatalf("domain comparison must be case-insensitive, got drift %v", d)
	}
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return string(s[0]-32) + s[1:] // ASCII upper of the first byte (gmail.com -> Gmail.com)
}

// A box reporting validation_type_for as an interface{} map (from /v1/config JSON) round-trips into
// map[string]string via the same shape the config client parses — guard the JSON compatibility.
func TestReportedShapeFromJSON(t *testing.T) {
	raw := `{"gmail.com":"mx","googlemail.com":"mx"}`
	var reported map[string]string
	if err := json.Unmarshal([]byte(raw), &reported); err != nil {
		t.Fatalf("validation_type_for must decode to map[string]string, got %v", err)
	}
	if reported["gmail.com"] != "mx" {
		t.Fatal("decode mismatch")
	}
}
