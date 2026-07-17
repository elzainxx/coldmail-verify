package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// This file: the brain's client for the fork's GET /v1/config (§4.6, closes GAP-3/OQ-11). The
// stock validation JSON embeds only a config SUBSET, so the brain's config_fingerprint could not
// cover the engine in P1 (it marked engine inputs "unconfigured"). P2 fetches the FULL effective
// config here and computes the CANONICAL fingerprint brain-side (§4.6 fingerprint authority — the
// box's own hash is advisory/cross-check only; the brain never trusts a box-computed hash).

// EffectiveConfig is the full verdict-affecting engine config the fork exposes (§4.6). Decoded as
// a generic map so an additive future engine option is captured without a code change (additive).
type EffectiveConfig struct {
	Raw map[string]any // the effective_config object, verbatim
}

// FetchConfig GETs <baseURL>/v1/config and returns the effective config. A non-2xx or unreadable
// config is an error: the brain must REFUSE to record a confident fingerprint (§4.6 failure mode)
// rather than silently mis-bucket — the caller flags the verdict fingerprint-degraded.
func (c *HTTPClient) FetchConfig(ctx context.Context, baseURL, token string) (EffectiveConfig, error) {
	url := trimSlash(baseURL) + "/v1/config"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return EffectiveConfig{}, fmt.Errorf("node: config request: %w", err)
	}
	req.Header.Set("Authorization", token)
	resp, err := c.hc.Do(req)
	if err != nil {
		return EffectiveConfig{}, &TransportError{Err: err}
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return EffectiveConfig{}, fmt.Errorf("node: read config: %w", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return EffectiveConfig{}, &HTTPStatusError{Status: resp.StatusCode, Body: body}
	}
	var envelope struct {
		EffectiveConfig map[string]any `json:"effective_config"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return EffectiveConfig{}, fmt.Errorf("node: decode config: %w", err)
	}
	if envelope.EffectiveConfig == nil {
		return EffectiveConfig{}, fmt.Errorf("node: /v1/config missing effective_config")
	}
	return EffectiveConfig{Raw: envelope.EffectiveConfig}, nil
}

// CanonicalFingerprint computes the brain's authoritative SHA-256 over a sorted-key serialization
// of the effective config (§4.6, mirroring P1 §3.2.5's canonical algorithm). This is the SINGLE
// canonical algorithm; the box's advisory hash is ignored. Prefixed cfg_sha256: to match P1.
func (ec EffectiveConfig) CanonicalFingerprint() string {
	keys := make([]string, 0, len(ec.Raw))
	for k := range ec.Raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(canonicalValue(ec.Raw[k]))
		b.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "cfg_sha256:" + hex.EncodeToString(sum[:])[:16]
}

// ValidationTypeFor extracts the engine's validation_type_for map (domain → validation-type) from
// the effective config (§5.1). It is the box's REPORTED provider-routing backstop — the brain
// compares it against the render of its authoritative policy source (provrouting.Drift) and logs a
// drift alarm on mismatch (GAP-19/OQ-P3-8). Returns an empty map if the box publishes none. Values
// that aren't string→string (a malformed engine config) are skipped defensively — a partial read
// still lets the drift check flag what's missing, never fabricates agreement.
func (ec EffectiveConfig) ValidationTypeFor() map[string]string {
	out := map[string]string{}
	raw, ok := ec.Raw["validation_type_for"]
	if !ok {
		return out
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return out
	}
	for domain, v := range m {
		if s, ok := v.(string); ok {
			out[domain] = s
		}
	}
	return out
}

// canonicalValue renders a config value deterministically (maps sorted, arrays sorted) so the
// fingerprint is stable regardless of JSON key/element order.
func canonicalValue(v any) string {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(t))
		for _, k := range keys {
			parts = append(parts, k+":"+canonicalValue(t[k]))
		}
		return "{" + strings.Join(parts, ",") + "}"
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			parts = append(parts, canonicalValue(e))
		}
		sort.Strings(parts)
		return "[" + strings.Join(parts, ",") + "]"
	default:
		return fmt.Sprintf("%v", v)
	}
}
