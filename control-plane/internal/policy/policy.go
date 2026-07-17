// Package policy is the signals-plus-policy engine (P1 §7). A policy is table-driven DATA
// (the open jsonb `definition` in the policies store, OQ-25) — "more policies slot in"
// without new code (§1.6). The engine runs the ordered layers, applies the policy's gates
// (cheapest-sufficient stop-early), folds flags into confidence, and produces the core
// verdict. Layers find; the POLICY decides (§7.1). The engine NEVER mints a value outside
// the frozen vocab (Door 4) and NEVER guesses `valid`/`invalid` — harden-toward-unknown.
package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/mnfi/codmailling/control-plane/internal/layers"
	"github.com/mnfi/codmailling/control-plane/internal/vocab"
)

// Definition is the parsed policy (P1 §7.2). It is loaded from the store's open jsonb, so
// unknown keys are tolerated (OQ-25) — later phases add keys (rate_limits P4, external P6,
// historical_bounce gate P7) without a shape change. Phase 1 reads the keys below.
type Definition struct {
	Key     string `json:"-"`
	Version int    `json:"-"`

	Order                        []string          `json:"order"`
	Gates                        map[string]string `json:"gates"`                            // "<layer>:<result>" -> "<status>:<reason>:<stop|continue>"
	Flags                        []string          `json:"flags"`                            // layers whose finding sets an is_* flag / nudges confidence
	RecommendationOverrides      map[string]string `json:"recommendation_overrides_by_reason"` // reason -> recommendation
	ProviderRouting              map[string]string `json:"provider_routing"`                 // mx_provider -> "mx" (P1: routes big providers to risky/provider_no_smtp)
	Confidence                   ConfidenceModel   `json:"confidence"`
	RecommendationThresholds     Thresholds        `json:"recommendation_thresholds"`
}

// ConfidenceModel is the P1 fixed baseline-by-reason table + flag deltas (OQ-8, §7.4). Not a
// learned model — there is no outcome data yet; P7 recalibrates per (reason × provider).
type ConfidenceModel struct {
	BaselineByReason map[string]int `json:"baseline_by_reason"`
	FlagDeltas       map[string]int `json:"flag_deltas"`
	Default          int            `json:"default"`
}

// Thresholds map (status × confidence) → recommendation (§3.1.3 directional invariants are
// LOCKED in vocab.ComposabilityOK; only these cut VALUES are a DIAL, §7.2).
type Thresholds struct {
	Send   int `json:"send"`   // confidence ≥ send  → send (only where status allows it)
	Review int `json:"review"` // confidence ≥ review → review; below → skip
}

// Parse builds a Definition from stored jsonb. Unknown keys are ignored (OQ-25 tolerate-unknown).
func Parse(key string, version int, raw []byte) (*Definition, error) {
	var d Definition
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("policy: parse %s v%d: %w", key, version, err)
	}
	d.Key, d.Version = key, version
	if len(d.Order) == 0 {
		return nil, fmt.Errorf("policy: %s v%d has empty order", key, version)
	}
	return &d, nil
}

// Verdict is the core policy output — the four Door-1 verdict fields plus the assembled
// flags/signals bags. The verdict assembler (Increment 6) wraps this with provenance into
// the full Door-1 Response + the LOG row. Kept vocab-typed so an illegal value can't escape.
type Verdict struct {
	Status         vocab.Status
	Reason         vocab.Reason
	Confidence     int
	Recommendation vocab.Recommendation
	Flags          map[string]bool
	Signals        map[string]any
	StoppedAt      string // layer id that produced the terminal gate ("" if none fired)
}

// action is a parsed "<status>:<reason>:<stop|continue>" gate value.
type action struct {
	status vocab.Status
	reason vocab.Reason
	stop   bool
}

func parseAction(s string) (action, error) {
	var a action
	var st, rs, mode string
	parts := splitColon(s)
	if len(parts) != 3 {
		return a, fmt.Errorf("policy: bad action %q (want status:reason:stop|continue)", s)
	}
	st, rs, mode = parts[0], parts[1], parts[2]
	a.status = vocab.Status(st)
	a.reason = vocab.Reason(rs)
	if !vocab.ValidStatus(a.status) {
		return a, fmt.Errorf("policy: action status %q not in vocab (Door 4)", st)
	}
	if !vocab.ValidReason(a.reason) {
		return a, fmt.Errorf("policy: action reason %q not in vocab (Door 4)", rs)
	}
	switch mode {
	case "stop":
		a.stop = true
	case "continue":
		a.stop = false
	default:
		return a, fmt.Errorf("policy: action mode %q (want stop|continue)", mode)
	}
	return a, nil
}

func splitColon(s string) []string {
	out := []string{}
	cur := ""
	for _, r := range s {
		if r == ':' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	return append(out, cur)
}

// Engine evaluates layer outputs against a policy. It holds no state; a single instance
// serves all requests.
type Engine struct{}

// NewEngine returns the stateless policy engine.
func NewEngine() *Engine { return &Engine{} }

// Evaluate runs the policy over the given layers against the evidence, cheapest-sufficient:
// it executes layers in policy `order`, checks each finding against `gates`, and STOPS at the
// first gate whose action says stop (§7.3). Flags fold into confidence. If no gate stops, the
// honest P1 outcome is risky/plausible (or risky/provider_no_smtp for :mx providers) — NEVER
// valid (SMTP is required for a confident yes; §7.3). The returned Verdict is vocab-valid and
// composability-correct by construction.
func (e *Engine) Evaluate(ctx context.Context, def *Definition, ev *layers.Evidence, layerSet map[string]layers.Layer) (Verdict, error) {
	flags := map[string]bool{"from_cache": false}
	signals := map[string]any{}
	flagLayers := toSet(def.Flags)

	for _, layerID := range def.Order {
		layer, ok := layerSet[layerID]
		if !ok {
			continue // a policy may name a layer not built this phase (SEAM); skip cleanly.
		}
		out := layer.Run(ctx, ev)
		recordSignal(signals, flags, out)

		// Gate check: does this (layer:result) map to an action?
		gateKey := out.LayerID + ":" + string(out.Result)
		if actStr, hasGate := def.Gates[gateKey]; hasGate {
			act, err := parseAction(actStr)
			if err != nil {
				return Verdict{}, err
			}
			if act.stop {
				return e.finish(def, act.status, act.reason, flags, signals, out.LayerID), nil
			}
		}
		_ = flagLayers // flags are recorded above; membership reserved for future weighting
	}

	// No gate fired → the honest P1 residue. Provider routing: a big provider routed to :mx
	// cannot be SMTP-confirmed in P1 → risky/provider_no_smtp (OQ-14). Otherwise risky/plausible.
	if prov, ok := signals["mx_provider"].(string); ok && prov != "" {
		if route, routed := def.ProviderRouting[prov]; routed && route == "mx" {
			return e.finish(def, vocab.StatusRisky, vocab.ReasonProviderNoSMTP, flags, signals, "provider_routing"), nil
		}
	}
	return e.finish(def, vocab.StatusRisky, vocab.ReasonPlausible, flags, signals, ""), nil
}

// finish assembles the terminal verdict: computes confidence from the reason baseline + flag
// deltas, derives the recommendation from (status × thresholds) honoring the LOCKED
// composability invariants, and returns a vocab-valid Verdict. Panics are impossible — an
// illegal combination is repaired toward the safe direction rather than emitted.
func (e *Engine) finish(def *Definition, status vocab.Status, reason vocab.Reason, flags map[string]bool, signals map[string]any, stoppedAt string) Verdict {
	conf := e.confidence(def, reason, flags)
	rec := e.recommend(def, status, reason, conf)
	return Verdict{
		Status: status, Reason: reason, Confidence: conf, Recommendation: rec,
		Flags: flags, Signals: signals, StoppedAt: stoppedAt,
	}
}

func (e *Engine) confidence(def *Definition, reason vocab.Reason, flags map[string]bool) int {
	c := def.Confidence.Default
	if v, ok := def.Confidence.BaselineByReason[string(reason)]; ok {
		c = v
	}
	// Fold flag deltas (deterministic order for reproducibility).
	keys := make([]string, 0, len(def.Confidence.FlagDeltas))
	for k := range def.Confidence.FlagDeltas {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if flags[k] {
			c += def.Confidence.FlagDeltas[k]
		}
	}
	return clamp(c, 0, 100)
}

// recommend maps (status × confidence) → recommendation, then REPAIRS toward the LOCKED
// composability invariant (vocab.ComposabilityOK) so an illegal pair can never be emitted:
//   invalid ⇒ skip; valid ⇒ send|review; risky/unknown ⇒ review|skip.
func (e *Engine) recommend(def *Definition, status vocab.Status, reason vocab.Reason, conf int) vocab.Recommendation {
	// Reason-level override (e.g. cold-email: disposable → skip) takes precedence, but is
	// still clamped to composability below.
	var rec vocab.Recommendation
	if ovr, ok := def.RecommendationOverrides[string(reason)]; ok {
		rec = vocab.Recommendation(ovr)
	} else {
		switch {
		case conf >= def.RecommendationThresholds.Send:
			rec = vocab.RecSend
		case conf >= def.RecommendationThresholds.Review:
			rec = vocab.RecReview
		default:
			rec = vocab.RecSkip
		}
	}
	return repairComposability(status, rec)
}

// repairComposability forces the recommendation into the LOCKED set for the status. It never
// UP-grades toward send; it only moves to the nearest allowed, safer action — so a threshold
// mis-set (a DIAL) can never produce the forbidden invalid+send, and never over-promises.
func repairComposability(status vocab.Status, rec vocab.Recommendation) vocab.Recommendation {
	if vocab.ComposabilityOK(status, rec) {
		return rec
	}
	switch status {
	case vocab.StatusInvalid:
		return vocab.RecSkip // the only legal invalid recommendation
	case vocab.StatusValid:
		return vocab.RecReview // valid but rec was skip → the safe legal value is review
	case vocab.StatusRisky, vocab.StatusUnknown:
		// send is illegal here → downgrade to review (the higher of the two legal values).
		return vocab.RecReview
	default:
		return vocab.RecReview
	}
}

// recordSignal writes a layer's finding into the signals/flags bags (the response evidence).
func recordSignal(signals map[string]any, flags map[string]bool, out layers.LayerOutput) {
	switch out.SignalKey {
	case "is_role", "is_disposable", "is_free_provider", "is_spam_trap", "has_typo":
		flags[flagNameFor(out.SignalKey)] = boolVal(out.Value)
		if out.SignalKey == "has_typo" && out.Detail != "" {
			signals["suggestion"] = out.Detail
		}
	case "mx_provider":
		if s, ok := out.Value.(string); ok && s != "" {
			signals["mx_provider"] = s
		}
	case "syntax_valid", "mx_found", "null_mx":
		signals[out.SignalKey] = boolVal(out.Value)
	default:
		if out.Value != nil {
			signals[out.SignalKey] = out.Value
		}
	}
}

func flagNameFor(signalKey string) string { return signalKey } // flags mirror signal keys 1:1 in P1

func boolVal(v any) bool {
	b, _ := v.(bool)
	return b
}

func toSet(ss []string) map[string]struct{} {
	m := make(map[string]struct{}, len(ss))
	for _, s := range ss {
		m[s] = struct{}{}
	}
	return m
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
