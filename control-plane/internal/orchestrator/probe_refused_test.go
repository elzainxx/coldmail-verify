package orchestrator

// ADR-0021 END TO END: the node's destination guard refused (the golden `probe_refused` body shared with the Ruby wrapper's specs),
// and Verify turns it into risky/non_public_mx — never invalid, never cached, node called exactly once, and the raw sidecar still
// passes Door 3 (the plaintext address redacted). The control is the same orchestrator and the same domain with an accepting node,
// which is valid/accepted: without it, "the node returned a refusal so it refused" would prove only that the fake node returns bytes.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coldmail-verify/control-plane/internal/hash"
	"github.com/coldmail-verify/control-plane/internal/layers"
	"github.com/coldmail-verify/control-plane/internal/policy"
	"github.com/coldmail-verify/control-plane/internal/router"
	"github.com/coldmail-verify/control-plane/internal/store"
	"github.com/coldmail-verify/control-plane/internal/vocab"
)

func evilTestOrch(t *testing.T, nodeBody []byte) (*Orchestrator, *store.MemStore, *policy.Definition, *atomic.Int32) {
	t.Helper()
	h, err := hash.NewHasher([]byte("probe-refused-pepper"))
	if err != nil {
		t.Fatalf("hasher: %v", err)
	}
	mem := store.NewMem(nil)
	mem.SeedNodeHealth(greenNode)
	def, _ := policy.ColdEmailDefinition()
	calls := &atomic.Int32{}
	nodes := []router.Node{{ID: "n1", BaseURL: "http://mock", TokenRef: "n1", Enabled: true, PublicEgressIP: testEgressIP}}
	res := fixtureResolver{mx: map[string][]layers.MXHost{"evil.test": {{Host: "mx.evil.test", Pref: 10}}}}
	o := New(Deps{
		Hasher: h, Store: mem, Resolver: res, CacheTTL: time.Hour, ConfigFingerprint: "cfg_sha256:test",
		SMTP: &SMTPDeps{
			Client: countingNode{body: nodeBody, calls: calls}, Registry: router.New(nodes, nil),
			TokenFor: func(string) string { return "tok" }, Health: mem, MaxAuditAge: 0,
		},
	})
	return o, mem, def, calls
}

func TestProbeRefused_TheNodesRefusalIsRiskyNonPublicMX_NeverInvalid_NeverCached(t *testing.T) {
	golden, err := os.ReadFile(filepath.Join("..", "..", "..", "verifier-node", "truemail-rack", "spec", "fixtures", "probe_refused.json"))
	if err != nil {
		t.Fatalf("golden fixture: %v", err)
	}
	o, mem, def, calls := evilTestOrch(t, golden)
	resp, err := o.Verify(context.Background(), "t@evil.test", def)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("the node was called %d times, want 1 (it is the node that refuses)", calls.Load())
	}
	if resp.Status == vocab.StatusInvalid {
		t.Fatal("a destination refusal must never make the address invalid")
	}
	if resp.Status != vocab.StatusRisky || resp.Reason != vocab.ReasonNonPublicMX {
		t.Fatalf("got %s/%s, want risky/non_public_mx", resp.Status, resp.Reason)
	}
	if resp.Recommendation != vocab.RecReview {
		t.Fatalf("recommendation = %s, want review", resp.Recommendation)
	}
	if b, _ := resp.Signals["probe_refused"].(bool); !b {
		t.Fatalf("signals.probe_refused missing: %v", resp.Signals)
	}
	if mem.CacheLen() != 0 {
		t.Fatalf("a refusal derived from current DNS state must not be cached, cache len = %d", mem.CacheLen())
	}
	if len(mem.Log) != 1 {
		t.Fatalf("the verdict must still be LOGged exactly once (Door 2), got %d", len(mem.Log))
	}
	// Door 3: the sidecar holds the node's body, with the plaintext address redacted.
	if len(mem.Raw) != 1 || strings.Contains(string(mem.Raw[0].Raw), "t@evil.test") {
		t.Fatalf("raw sidecar rows=%d; the plaintext address must be redacted from the stored node body", len(mem.Raw))
	}
}

func TestProbeRefused_Control_AnAcceptingNodeOnTheSameDomainIsValid(t *testing.T) {
	body := []byte(`{
	  "wrapper_api_version":"v1","email":"t@evil.test","validation_type":"smtp","success":true,"smtp_debug":null,
	  "catchall":{"probed":true,"probe_local_part":"nx","accepted":false,"conclusion":"not_catchall"},
	  "mx":{"domain":"evil.test","mail_servers":["93.184.216.34"]}}`)
	o, _, def, calls := evilTestOrch(t, body)
	resp, err := o.Verify(context.Background(), "t@evil.test", def)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if calls.Load() != 1 || resp.Status != vocab.StatusValid || resp.Reason != vocab.ReasonAccepted {
		t.Fatalf("CONTROL FAILED: an accepting node gave %s/%s after %d call(s), want valid/accepted — the refusal test above proves nothing", resp.Status, resp.Reason, calls.Load())
	}
}
