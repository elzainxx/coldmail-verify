// Package provrouting is the Phase-3 provider-routing single source (§5.1, resolves GAP-19/OQ-P3-8).
//
// Provider :mx routing now lives conceptually in TWO places — the brain (skips the SMTP call for a
// :mx-classified provider) and the engine (its own validation_type_for does :mx as defense in
// depth). Two independent tables are a classic drift bug. The LOCKED mechanism (§5.1, PM1): there
// is ONE editable source — the policy's provider_routing map (provider-class → "mx", P1 §7.2) — and
// the engine's validation_type_for is a GENERATED render of it (domain → "mx"). The brain is
// authoritative for whether the SMTP call happens at all; the engine map is a synced backstop. This
// package renders the engine map from the policy source and asserts the box's /v1/config matches —
// converting "two tables that might drift" into "one source, one generated copy, one equality check."
//
// It is brain-side config GENERATION + a drift ALARM, not a mutator: in P3 the box's config is set
// by its own ENV (pull model, OQ-P2-2), so a mismatch is LOGGED (a drift alarm an operator acts on),
// never silently reconciled and never a fabricated verdict. Additive-only: no door, no vocab change.
package provrouting

import (
	"sort"
	"strings"
)

// providerDomains maps a provider CLASS (the policy's provider_routing key — the same buckets the
// cheap provider_classify layer emits: google|yahoo|o365) to its canonical consumer domains. This
// is the DIAL list (§5.1 "the exact provider→:mx list is tunable and grows"); the PRINCIPLE (big
// consumer providers are never SMTP'd from our IP) is LOCKED. O365 stays :mx for the first box
// (OQ-P3-9 affirmed — no hunch-based :smtp special-casing; revisit only on P7 bounce data).
var providerDomains = map[string][]string{
	"google": {"gmail.com", "googlemail.com"},
	"yahoo":  {"yahoo.com", "yahoo.co.uk", "ymail.com", "rocketmail.com"},
	"o365":   {"outlook.com", "hotmail.com", "live.com", "msn.com", "office365.com"},
}

// Render generates the engine's validation_type_for map (domain → validation-type) from the policy's
// provider_routing map (provider-class → validation-type). Only providers the policy routes to a
// non-empty type are expanded; each provider's canonical domains all get that type. The result is
// what the box's validation_type_for SHOULD equal (the generated backstop). Deterministic.
//
// Example: provider_routing {google:"mx", yahoo:"mx", o365:"mx"} renders
// {gmail.com:"mx", googlemail.com:"mx", yahoo.com:"mx", ..., outlook.com:"mx", ...}.
func Render(providerRouting map[string]string) map[string]string {
	out := map[string]string{}
	for provider, vtype := range providerRouting {
		if vtype == "" {
			continue
		}
		for _, domain := range providerDomains[provider] {
			out[strings.ToLower(domain)] = vtype
		}
	}
	return out
}

// Drift compares the EXPECTED render (from the policy source) against the box's REPORTED
// validation_type_for (parsed from /v1/config, §4.6). It returns a sorted, human-readable list of
// mismatches — a domain the brain expects to be :mx-routed that the box does NOT route that way, or
// a domain the box routes that the brain didn't render (an un-tracked engine override). An empty
// slice means the box's backstop is in sync with the single source. The caller LOGS a non-empty
// result as a drift alarm (§5.1) — it does not auto-reconcile (pull model; the box owns its config).
//
// reported values are compared case-insensitively on the domain key (validation_type_for keys are
// domains, norm/v1 lowercased). A reported value's TYPE must also match the expected type.
func Drift(expected, reported map[string]string) []string {
	norm := func(m map[string]string) map[string]string {
		n := make(map[string]string, len(m))
		for k, v := range m {
			n[strings.ToLower(strings.TrimSpace(k))] = strings.ToLower(strings.TrimSpace(v))
		}
		return n
	}
	exp, rep := norm(expected), norm(reported)

	var drifts []string
	// Every expected mapping must be present on the box with the same type.
	for domain, vtype := range exp {
		got, ok := rep[domain]
		if !ok {
			drifts = append(drifts, "missing on box: "+domain+"->"+vtype)
			continue
		}
		if got != vtype {
			drifts = append(drifts, "type mismatch: "+domain+" brain="+vtype+" box="+got)
		}
	}
	// A box override for a provider domain the brain tracks but did NOT render (e.g. the box routes a
	// consumer domain to :smtp) is a drift too — the brain would send it for SMTP thinking the box
	// agrees. Only flag domains the brain KNOWS about (a box may legitimately have extra corp
	// overrides we don't manage); we track the union of all provider domains as "managed".
	managed := managedDomains()
	for domain, vtype := range rep {
		if _, isExpected := exp[domain]; isExpected {
			continue // already checked above
		}
		if managed[domain] {
			drifts = append(drifts, "unexpected box override on a managed provider domain: "+domain+"->"+vtype)
		}
	}
	sort.Strings(drifts)
	return drifts
}

// managedDomains is the set of all provider domains this package knows about (the union of
// providerDomains). A drift on one of these matters; a box override on an unrelated corporate domain
// is the operator's business, not this check's.
func managedDomains() map[string]bool {
	m := map[string]bool{}
	for _, domains := range providerDomains {
		for _, d := range domains {
			m[strings.ToLower(d)] = true
		}
	}
	return m
}

// ManagedProviders returns the provider classes this package can expand (for docs/diagnostics).
func ManagedProviders() []string {
	ps := make([]string, 0, len(providerDomains))
	for p := range providerDomains {
		ps = append(ps, p)
	}
	sort.Strings(ps)
	return ps
}
