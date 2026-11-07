// Command control-plane is the control-plane entrypoint. Phase 0: load config + secrets, wire
// the hasher and the HTTP front door, and serve. It builds and runs empty — no real
// verification. It proves: config loads (manifest flags readable), the secrets scaffold
// works, the kill-switch defaults to cheap-layers-only, and the Door-1 endpoint responds.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/coldmail-verify/control-plane/internal/api"
	"github.com/coldmail-verify/control-plane/internal/audit"
	"github.com/coldmail-verify/control-plane/internal/config"
	"github.com/coldmail-verify/control-plane/internal/external"
	"github.com/coldmail-verify/control-plane/internal/flywheel"
	"github.com/coldmail-verify/control-plane/internal/hash"
	"github.com/coldmail-verify/control-plane/internal/health"
	"github.com/coldmail-verify/control-plane/internal/node"
	"github.com/coldmail-verify/control-plane/internal/orchestrator"
	"github.com/coldmail-verify/control-plane/internal/policy"
	"github.com/coldmail-verify/control-plane/internal/provrouting"
	"github.com/coldmail-verify/control-plane/internal/queue"
	"github.com/coldmail-verify/control-plane/internal/ratelimit"
	"github.com/coldmail-verify/control-plane/internal/router"
	"github.com/coldmail-verify/control-plane/internal/secrets"
	"github.com/coldmail-verify/control-plane/internal/store"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	cfg, err := config.LoadFromOS()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	log.Printf("control-plane starting: %s", cfg.Summary())

	// Secrets: at Phase 0 we do NOT require core secrets so `control-plane` can boot for a build
	// smoke without a real pepper. A real (feature-serving) deployment sets requireCore=true.
	requireCore := cfg.Env == "production"
	sec, err := secrets.Load(secrets.EnvSource{}, requireCore)
	if err != nil {
		log.Fatalf("secrets: %v", err)
	}
	log.Printf("secrets: %s", sec.String()) // redacted — presence only

	// Hasher (Door 3) is wired only if a pepper is present; otherwise readiness reports not-ready.
	var hasher *hash.Hasher
	if sec.HasPepper() {
		p, _ := sec.Pepper()
		if hasher, err = hash.NewHasher(p); err != nil {
			log.Fatalf("hash: %v", err)
		}
	} else {
		log.Printf("hash: no pepper loaded — /readyz will report not-ready (a build smoke is fine)")
	}

	// appCtx lives for the whole process (cancelled on shutdown). The P3 re-audit ticker rides it so
	// it stops cleanly on exit; setup DB ops use their own short timeout.
	appCtx, appCancel := context.WithCancel(context.Background())
	defer appCancel()

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           buildAPI(appCtx, cfg, sec, hasher).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	go func() {
		log.Printf("listening on %s", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	log.Printf("stopped")
}

// buildAPI wires the front door. If a database is configured AND a pepper is present, it
// attaches the real orchestrator (the cheap-layers verify lifecycle). Otherwise it returns a
// front door with NO verifier — /v1/verify honestly reports 503 (not ready) rather than
// fabricating a verdict. Cheap layers only; no port 25 (Phase 1).
func buildAPI(appCtx context.Context, cfg config.Config, sec *secrets.Secrets, hasher *hash.Hasher) *api.Server {
	srv := api.New(cfg, sec, hasher)
	if hasher == nil || cfg.DatabaseURL == "" {
		log.Printf("verify: no %s configured or no pepper — /v1/verify will report not-ready (503)",
			"TM_DATABASE_URL")
		return srv
	}
	// Setup DB ops use a short timeout; the long-lived appCtx is passed to buildSMTP for the ticker.
	ctx, cancel := context.WithTimeout(appCtx, 10*time.Second)
	defer cancel()
	pg, err := store.OpenPG(ctx, cfg.DatabaseURL)
	if err != nil {
		// A configured-but-unreachable DB is a real error, but the control-plane can still SERVE
		// health and readiness — and since item 10, readiness ANSWERS not_ready here (the pinger is
		// never attached on this path, so /readyz reports db:false → 503). Verify stays 503 too.
		// Health stays 200: the process is alive, which is the only thing liveness claims.
		log.Printf("verify: store unavailable (%v) — /v1/verify will report not-ready (503)", err)
		return srv
	}
	// ADR-0017: keep verification_raw's future partitions created. Started HERE — as soon as the pool is
	// up and before any of the early returns below — because the partition cliff is a property of the
	// DATABASE, not of whether this process wired /v1/verify: a control-plane that came up half-wired
	// must still heal the table. Rides appCtx, so it lives for the process and stops on shutdown.
	startRawPartitionMaintenance(appCtx, cfg, pg)
	def, err := policy.ColdEmailDefinition()
	if err != nil {
		log.Printf("verify: policy load failed (%v) — /v1/verify not wired", err)
		return srv
	}
	// Seed the code-owned default policy into the store if absent (idempotent, §4.6).
	if err := pg.EnsurePolicy(ctx, policy.ColdEmailKey, policy.ColdEmailVersion,
		[]byte(policy.ColdEmailDefinitionJSON), true); err != nil {
		log.Printf("verify: could not ensure default policy (%v) — /v1/verify not wired", err)
		return srv
	}
	// Phase 2/3: wire the SMTP stage IF SMTP is enabled (kill-switch not Rung 1) AND a node list
	// exists. Otherwise the orchestrator stays cheap-layers-only (P1 steady state / Rung 3). P3 also
	// registers each node in the `nodes` table (provisioning) + starts the minimal re-audit ticker
	// (which rides appCtx so it lives for the whole process, not the 10s setup ctx).
	smtpDeps, cfgFingerprint := buildSMTP(ctx, appCtx, cfg, sec, pg)

	// P6 (§3): wire the external-fallback stage IFF it is deliberately enabled (Fallback==enabled AND
	// the §7.4 DPA confirmed) AND a vendor key is loaded. Otherwise externalDeps is nil → the layer
	// is fully OFF (the P1–P5 posture, the default). buildExternal enforces the "enabling is hard"
	// discipline: three independent affirmative switches, or no egress. The global-fallback-kill
	// (Fallback==off) forces it off regardless (ExternalFallbackEnabled returns false).
	externalDeps := buildExternal(cfg, sec)

	// P7 (§4.6/§5.4): wire the flywheel's two LIVE-PATH touches (the historical-bounce gate + the
	// calibrated-confidence read). Both default OFF (opt-in flags) so a fresh P7 system behaves
	// identically to end-of-P6 (the DR-2 fresh-system invariant); even when enabled, the read only
	// overrides on an APPROVED calibration version, and the gate only fires on a confirmed ≥N in-TTL
	// history. The recalibration PASS (the writer) is a separate scheduled job (recommend-only by
	// default) — not started here; the live touches read what an approved pass produced.
	flywheelDeps := buildFlywheel(cfg)

	orch := orchestrator.New(orchestrator.Deps{
		Hasher:            hasher,
		Store:             pg,
		CacheTTL:          cfg.CacheTTL(),
		ConfigFingerprint: cfgFingerprint,
		SMTP:              smtpDeps,
		External:          externalDeps,
		Flywheel:          flywheelDeps,
	})
	mode := "cheap-layers-only"
	if smtpDeps != nil {
		mode = "SMTP-enabled (against node list)"
	}
	log.Printf("verify: orchestrator wired (%s, policy=%s v%d, cache_ttl=%s)",
		mode, def.Key, def.Version, cfg.CacheTTL())
	// PN-13 (§7.5): wire per-caller front-door auth. The store's LookupCaller reads api_callers
	// (SELECT-only for cp_runtime; issuance/rotation are cp_admin/DR-3). Once wired, a valid
	// per-caller key authenticates any endpoint and the P5 bulk surface requires one; the legacy
	// static token still authenticates the sync /v1/verify during transition (backward-compat).
	// WithPinger (fix-plan item 10): the SAME *store.PG, handed to /readyz as its readiness probe.
	// It is attached HERE, at the end of wiring, rather than next to OpenPG. Four early returns sit
	// above this line; two of them (policy load, default-policy seed) happen with a perfectly
	// reachable pg, and on those paths /v1/verify is NOT wired and answers 503. Pinging from OpenPG
	// would make readiness say `db:true, ready` on exactly those paths — the readiness-vs-serving
	// drift this item exists to remove, just relocated. Attaching it only once the serving path is
	// assembled means all four early returns leave the pinger nil, and a nil pinger reports
	// db:false → not_ready, which is the honest answer in every one of those states.
	srv = srv.WithVerifier(orch, def).WithCallerAuth(pg).WithPinger(pg)

	// P5 queue (§3/§7): wire the bulk endpoint + start the drainer, IFF the DR-4 key is present.
	// The queue holds plaintext at rest (the one bounded Door-3 exception), so it MUST NOT start
	// without its at-rest encryption key — a queue-enabled brain with no key is honestly not-ready
	// (the bulk endpoints return 503), never silently storing plaintext. The drainer reuses the
	// SAME orch (its ComputeForQueue runs the identical P4 gates — §8 master invariant).
	buildQueue(appCtx, cfg, sec, pg, orch, hasher, def, srv, cfgFingerprint)
	return srv
}

// buildQueue wires the P5 bulk submitter + starts the drainer on appCtx, but ONLY when the DR-4
// at-rest key (TM_QUEUE_ADDR_KEY) is loaded. Without it the queue subsystem refuses to start
// (harden-toward-safety: never store the queue's plaintext exception unencrypted) — the bulk
// endpoints stay 503. The lease TTL must exceed the brain's max SMTP deadline (GAP-37); NewDrainer
// fails fast otherwise and we log + skip the queue rather than run with a race-prone lease.
func buildQueue(appCtx context.Context, cfg config.Config, sec *secrets.Secrets, pg *store.PG, orch *orchestrator.Orchestrator, hasher *hash.Hasher, def *policy.Definition, srv *api.Server, cfgFingerprint string) {
	key, prev, ok := sec.QueueAddrKey()
	if !ok {
		log.Printf("queue: %s not set — bulk endpoint disabled (won't store plaintext without the DR-4 key)", secrets.EnvQueueAddrKey)
		return
	}
	cipher, err := queue.NewAddrCipher(key, prev)
	if err != nil {
		log.Printf("queue: DR-4 cipher init failed (%v) — bulk endpoint disabled", err)
		return
	}
	bulkMax := 0 // 0 => the handler's built-in safe hard cap applies (§7.5 dial is owner-OQ/UNSET)
	if cfg.Dials.QueueBulkMaxAddresses.Set {
		bulkMax = cfg.Dials.QueueBulkMaxAddresses.Value
	}
	enq := &queue.Enqueuer{Store: pg, Hasher: hasher, Cipher: cipher, ConfigFingerprint: cfgFingerprint}
	srv.WithBulk(enq, pg, bulkMax)

	// Backoff dials (§4.2) — ALL read from config now (promoted from code-defaults; PROJECT_RULES §6).
	bo := queue.Backoff{
		Base:       cfg.QueueBackoffBase(),
		Factor:     float64(cfg.Dials.QueueBackoffFactor),
		Cap:        cfg.QueueBackoffCap(),
		JitterFrac: float64(cfg.Dials.QueueBackoffJitterPct) / 100.0,
		RateWait:   cfg.QueueRateWait(),
	}
	// GAP-37: the lease TTL must EXCEED the brain's max SMTP client deadline so a live attempt never
	// loses its lease. maxSMTP falls back to 60s when the deadline dial is UNSET (X-7 coupling); the
	// TTL adds the configured margin (Validate() guarantees the margin > 0, so TTL > deadline).
	maxSMTP := cfg.SMTPClientDeadline()
	if maxSMTP <= 0 {
		maxSMTP = 60 * time.Second
	}
	leaseTTL := cfg.QueueLeaseTTL(maxSMTP)
	drainer, err := queue.NewDrainer(queue.Deps{
		Store: pg, Compute: orch, Policy: policyResolver{def: def}, Cipher: cipher, Backoff: bo,
		WorkerID: "drainer-1", LeaseTTL: leaseTTL, MaxSMTPDeadline: maxSMTP,
		MaxAttempts: cfg.Dials.QueueMaxAttempts, MaxRetryAge: cfg.QueueMaxRetryAge(),
		BatchSize: cfg.Dials.QueueDrainBatchSize, IdlePoll: cfg.QueueDrainIdlePoll(),
		// Kill-switch Rung 4 (fix-plan item 4 / D2): accept-and-defer means the front door keeps
		// accepting and storing work while THE DRAINER IS STOPPED. This is the line that makes the
		// rung real — before it, TM_FRONTDOOR_MODE=accept_and_defer validated and did nothing.
		// A closure over cfg, not a bool, so the decision is made per tick.
		Paused: func() bool { return cfg.KillSwitch.VerdictProductionPaused() },
	})
	if err != nil {
		log.Printf("queue: drainer init failed (%v) — bulk submissions will enqueue but NOT drain until fixed", err)
		return
	}
	log.Printf("queue: bulk endpoint wired; drainer starting (lease_ttl=%s max_smtp=%s max_attempts=%d max_age=%s)",
		leaseTTL, maxSMTP, cfg.Dials.QueueMaxAttempts, cfg.QueueMaxRetryAge())
	if cfg.KillSwitch.VerdictProductionPaused() {
		// Loud at boot, not just at the first tick: starting a brain that is ALREADY at Rung 4 is a
		// deliberate act, and the operator must see the process agree with the config it was handed.
		log.Printf("queue: drainer starts PAUSED (frontdoor=%s, kill-switch Rung 4) — bulk submissions are accepted and stored durably; NOTHING will be processed until the rung is released",
			cfg.KillSwitch.FrontDoor)
	}
	go drainer.Run(appCtx)
}

// buildExternal wires the P6 third-party-fallback stage — but ONLY when it is DELIBERATELY enabled:
// the kill-switch fallback rung is ON (Fallback==enabled — DEFAULT off) AND the §7.4 DPA is confirmed
// (TM_EXTERNAL_DPA_CONFIRMED=true — DEFAULT false) AND a vendor key is loaded. Any switch missing =>
// nil => the layer is fully OFF (the P1–P5 posture, the safe default). This is the "enabling is hard"
// discipline: enabling pre-DPA is unlawful sub-processing, so it takes THREE independent affirmative
// operator actions, and the global-fallback-kill (Fallback==off) overrides all of them.
//
// PENDING-REAL-VENDOR: this phase ships NO real HTTP vendor client — the real vendor is not enabled
// (GAP-47: vendor accuracy on the residue is untestable pre-integration). A configured real vendor
// is a swappable adapter behind external.Vendor (§8); the mock proves the mechanics locally. Until a
// real adapter is added, buildExternal wires a DISABLED-by-safety stub even when the switches are on,
// logging loudly — so a misconfiguration can't silently egress to a non-existent vendor.
func buildExternal(cfg config.Config, sec *secrets.Secrets) *orchestrator.ExternalDeps {
	if !cfg.ExternalFallbackEnabled() {
		// The common, safe path: fallback off (default), or DPA not confirmed. No egress, ever.
		if cfg.KillSwitch.Fallback == config.FallbackOff {
			log.Printf("external: fallback OFF (kill-switch Rung 2 / global-fallback-kill) — no vendor egress")
		} else {
			log.Printf("external: fallback layer disabled (DPA not confirmed: set TM_EXTERNAL_DPA_CONFIRMED=true only AFTER the §7.4 checklist passes — pre-DPA egress is unlawful sub-processing)")
		}
		return nil
	}
	// Enabled + DPA confirmed: a vendor key MUST be present for the named vendor, or we stay off.
	def, err := policy.ColdEmailDefinition()
	if err != nil || def.External == nil || def.External.Vendor == "" {
		log.Printf("external: no external policy block/vendor configured — fallback stays off")
		return nil
	}
	vendorName := def.External.Vendor
	if _, ok := sec.VendorKey(vendorName); !ok {
		log.Printf("external: %s enabled + DPA confirmed but no vendor API key for %q (set TM_VENDOR_KEYS) — fallback stays off (honest not-ready, never a fake vendor)", secrets.EnvVendorKeys, vendorName)
		return nil
	}
	// The budget + breaker DIALs are mapped from config here (so the §5 dials are genuinely CONSUMED,
	// never settable-but-ignored). This is the exact wiring a real adapter uses; only the vendor
	// itself is missing this phase.
	deps := &orchestrator.ExternalDeps{
		Vendor:           nil, // PENDING-REAL-VENDOR — a real external.Vendor adapter slots in HERE (§8)
		Budget:           configToBudget(cfg),
		Breaker:          configToBreaker(cfg),
		GloballyDisabled: cfg.KillSwitch.Fallback == config.FallbackOff, // global-fallback-kill
	}
	_ = deps // built to prove the config mapping; not returned until a real adapter exists (below).
	// A real adapter is not built this phase (PENDING-REAL-VENDOR). Refuse to wire a live-looking
	// stage against a non-existent HTTP client — that would be faking green. The mechanics are proven
	// in tests against external.MockVendor; a real adapter slots into deps.Vendor above to go live.
	log.Printf("external: PENDING-REAL-VENDOR — fallback is enabled + DPA confirmed + key present, but no real vendor adapter ships this phase (GAP-47). The layer is built + fail-verified locally against the mock; set deps.Vendor to a real external.Vendor adapter (budget=%d/day breaker_cooldown=%s) to go live.",
		cfg.Dials.ExternalDailyCap.Value, cfg.ExternalBreakerCooldown())
	return nil
}

// buildFlywheel wires the P7 live-path touches (§4.6 historical-bounce gate + §5.4 calibrated read).
// It is ALWAYS returned non-nil once a store is up (so the gate params + mode are readable), but BOTH
// touches default OFF via the opt-in flags — a fresh P7 system reads the P1 baseline + never gates
// (the DR-2 fresh-system invariant). The dials are CONSUMED here (never settable-but-ignored). This is
// the P7 analogue of buildExternal's config mapping — the mechanism is wired; enabling it is explicit.
func buildFlywheel(cfg config.Config) *orchestrator.FlywheelDeps {
	deps := &orchestrator.FlywheelDeps{
		GateEnabled:        cfg.FlywheelGateEnabled,
		CalibrationEnabled: cfg.FlywheelCalibrationEnabled,
		GateParams: flywheel.HistoricalGateParams{
			TTL:            cfg.HistoricalBounceTTL(),
			MinIndependent: cfg.Dials.HistoricalBounceMinN,
			PenaltyPoints:  cfg.Dials.HistoricalBouncePenalty,
		},
	}
	switch {
	case deps.GateEnabled && deps.CalibrationEnabled:
		log.Printf("flywheel: historical-bounce gate ON (ttl=%s minN=%d) + calibrated-read ON — verdicts read approved calibration; the gate short-circuits on confirmed ≥N in-TTL history",
			cfg.HistoricalBounceTTL(), cfg.Dials.HistoricalBounceMinN)
	case deps.CalibrationEnabled:
		log.Printf("flywheel: calibrated-read ON (gate OFF) — verdicts read an APPROVED calibration version; cold-start falls back to the P1 baseline")
	case deps.GateEnabled:
		log.Printf("flywheel: historical-bounce gate ON (ttl=%s minN=%d); calibrated-read OFF (P1 baseline confidence)", cfg.HistoricalBounceTTL(), cfg.Dials.HistoricalBounceMinN)
	default:
		log.Printf("flywheel: live-path touches OFF (fresh-system default) — behavior identical to end-of-P6; recalibration runs recommend-only and an owner approves + enables the reads to go live")
	}
	return deps
}

// configToBudget maps the §5.2 spend-ceiling dials to external.Budget. An UNSET dial (owner-OQ) →
// 0 → "not enforced" (never "unbounded" — the layer only egresses when deliberately enabled). This
// is where the TM_EXTERNAL_*_CAP / RATE_CEILING dials are consumed (not settable-but-ignored).
func configToBudget(cfg config.Config) external.Budget {
	return external.Budget{
		DailyCap:    cfg.Dials.ExternalDailyCap.Value,   // 0 when UNSET (Dial.Value is 0 unless Set)
		MonthlyCap:  cfg.Dials.ExternalMonthlyCap.Value,
		PerJobCap:   cfg.Dials.ExternalPerJobCap.Value,
		RateCeiling: cfg.Dials.ExternalRateCeiling.Value, // §5.2 calls/min burst brake — now consumed
	}
}

// configToBreaker maps the §5.3 circuit-breaker dials to external.BreakerParams.
func configToBreaker(cfg config.Config) external.BreakerParams {
	return external.BreakerParams{
		ErrorRateThreshold: float64(cfg.Dials.ExternalErrorRatePct) / 100.0,
		MinSamples:         cfg.Dials.ExternalErrorMinSamples,
		Cooldown:           cfg.ExternalBreakerCooldown(),
	}
}

// policyResolver adapts the single default policy to queue.PolicyResolver. P5 ships one policy (as
// P1 did); a request for a different (key,version) is not-found. When multi-policy lands, this
// resolves from the policy store instead.
type policyResolver struct{ def *policy.Definition }

func (p policyResolver) Resolve(_ context.Context, key string, version int) (*policy.Definition, error) {
	if p.def != nil && key == p.def.Key && version == p.def.Version {
		return p.def, nil
	}
	return nil, fmt.Errorf("queue: unknown policy %s v%d", key, version)
}

// buildSMTP wires the Phase-2 SMTP stage when SMTP is enabled and a node list exists. It fetches
// the fork's /v1/config (§4.6, GAP-3) from the first eligible node and computes the CANONICAL
// config_fingerprint over the effective engine config — retiring the P1 "engine=unconfigured"
// placeholder. If SMTP is off, no node exists, or the config fetch fails, it returns
// (nil, P1-fingerprint): the brain degrades to cheap-layers-only rather than SMTP on an unknown
// engine config (harden-toward-unknown / §4.6 "refuse a confident fingerprint" on failure).
func buildSMTP(ctx, appCtx context.Context, cfg config.Config, sec *secrets.Secrets, pg *store.PG) (*orchestrator.SMTPDeps, string) {
	fallback := cfg.ConfigFingerprint() // P1 engine=unconfigured fingerprint
	if !cfg.SMTPEnabled() || len(cfg.Nodes) == 0 {
		return nil, fallback
	}
	tokenFor := func(id string) string {
		if t, ok := sec.NodeToken(id); ok {
			return t
		}
		return ""
	}
	client := node.NewHTTPClient(cfg.SMTPClientDeadline())

	// §4: the per-provider rate limiter (durable budget in Postgres, ephemeral in-flight in-process).
	// Its caps come from the policy rate_limits key (§4.7, global-with-override OQ-P4-5). §3.2/§3.4:
	// the router's Strategy is now sticky-by-domain (HRW) with a least-inflight fallback; it consults
	// the limiter for per-node rate headroom (§3.3) + in-flight ranking (§3.4). This is the "one
	// mechanism in two parts" — sticky concentrates, the limiter paces (§4.1). A nil rate_limits key
	// makes the limiter inert (no rate limiting), and the strategy degrades to pure sticky affinity.
	var rateLimits *policy.RateLimits
	if def, derr := policy.ColdEmailDefinition(); derr == nil {
		rateLimits = def.RateLimits
	}
	limiter := ratelimit.New(ratelimit.Config{Budget: pg, Limits: rateLimits})
	sticky := router.NewSticky(
		func(rctx context.Context, nodeID string, plan router.Plan) bool {
			ok, herr := limiter.HasHeadroom(rctx, nodeID, ratelimit.BucketForDomain(plan.Provider, plan.Domain))
			return herr == nil && ok // a headroom-read error fails closed (no headroom → not this home)
		},
		func(nodeID string, plan router.Plan) int {
			return limiter.Inflight(nodeID, ratelimit.BucketForDomain(plan.Provider, plan.Domain))
		},
	)
	reg := router.New(cfg.Nodes, sticky)

	// The bootstrap /v1/config fetch just needs SOME configured node's base URL; use the first enabled
	// one directly (the eligibility filter that gates :smtp is applied per request via SMTPRoute, not
	// here). This keeps N=1 and N>1 identical — the config-fetch node is a bootstrap detail.
	var n config.NodeDescriptor
	for _, nd := range cfg.Nodes {
		if nd.Enabled {
			n = nd
			break
		}
	}
	if n.ID == "" {
		log.Printf("verify: SMTP enabled but no enabled node in the config list — staying cheap-layers-only")
		return nil, fallback
	}

	// §4.6: fingerprint the engine's effective config. On failure, refuse a confident engine
	// fingerprint — degrade to cheap-only (don't run SMTP against an unknown config).
	fpCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ec, ferr := client.FetchConfig(fpCtx, n.BaseURL, tokenFor(n.ID))
	if ferr != nil {
		log.Printf("verify: /v1/config fetch failed (%v) — cheap-layers-only (won't SMTP on an unknown engine config)", ferr)
		return nil, fallback
	}
	fingerprint := cfg.EngineConfigFingerprint(ec.CanonicalFingerprint())
	log.Printf("verify: SMTP node=%s config_fingerprint=%s", n.ID, fingerprint)

	// §5.1/GAP-19/OQ-P3-8: provider :mx routing has ONE source (the policy provider_routing map);
	// the engine's validation_type_for is a GENERATED render of it. Assert the box's reported
	// validation_type_for matches the render — a mismatch is a LOGGED drift alarm (the brain is
	// authoritative; the box's map is a synced backstop), never silent divergence, never auto-fix.
	if def, derr := policy.ColdEmailDefinition(); derr == nil {
		expected := provrouting.Render(def.ProviderRouting)
		if drifts := provrouting.Drift(expected, ec.ValidationTypeFor()); len(drifts) > 0 {
			log.Printf("verify: WARNING provider-routing drift (engine validation_type_for out of sync with policy source, §5.1): %v", drifts)
		} else {
			log.Printf("verify: provider-routing backstop in sync (validation_type_for matches policy source)")
		}
	}

	// §3.6/§4.5: register each configured node in the `nodes` table as `provisioning` (config-list
	// is authoritative — P1 OQ-5; this mirrors it + gives the audit summary a row to update). The
	// node stays provisioning → NOT :smtp-eligible until the first green audit ∧ port25_open. NOTE:
	// port25_open (§3.4 egress smoke test) is PENDING-REAL-BOX — false here in the local build, so no
	// node is :smtp-eligible until a real host proves egress-25 and audits green. The eligibility
	// LOGIC that consumes port25_open is fully built + tested (audit/router tests); only the real
	// value is deferred. Harden-toward-unknown: an unregisterable node just isn't eligible.
	for _, nd := range cfg.Nodes {
		if !nd.Enabled {
			continue
		}
		if rerr := pg.UpsertNodeState(ctx, nd.ID, nd.BaseURL, "provisioning", false); rerr != nil {
			log.Printf("verify: WARNING could not register node %s (%v) — it will not be :smtp-eligible", nd.ID, rerr)
		}
	}

	// §5: start the CONTINUOUS health-watcher (the P4 control loop). It REPLACES the P3 minimal
	// re-audit ticker (§1.2): it owns the audit cadence AND adds liveness polling + block-rate-driven
	// quarantine, with the three guards (sample gate, hysteresis, fleet-wide brake) + canary recovery.
	// The block-rate (statistical, A1) trigger is recommend-only by default (DR-2 shadow-mode) — the
	// watcher performs ZERO live block-rate mutation until an owner promotes A1 to auto (the
	// fresh-system invariant). Audit-red (A2) + canary (A3) stay auto. Runs on the process-lifetime
	// appCtx (cancelled on shutdown), the single writer of nodes.status (§5.1).
	startHealthWatcher(appCtx, cfg, client, pg, tokenFor)

	return &orchestrator.SMTPDeps{
		Client:         client,
		Registry:       reg,
		TokenFor:       tokenFor,
		SafeCheckOn:    false, // OQ-P2-7: smtp_safe_check OFF by default
		ClientDeadline: cfg.SMTPClientDeadline(),
		// Phase 3 (§4.4/§4.5/§5.2): the eligibility gate reads health from the store; MaxAuditAge is
		// the staleness window; WarmupCap is the per-node/per-day ascending :smtp ceiling.
		Health:        pg,
		MaxAuditAge:   cfg.AuditMaxAge(),
		WarmupCap:     cfg.Dials.WarmupDailyCap,
		CanaryCap:     cfg.Dials.CanaryWarmupCap, // §5.4: the LOW cap a recovering canary node is metered by
		CanarySampleN: cfg.Dials.CanarySampleN,   // §5.4/GAP-30: canary domain-sample rate (real samples in a healthy pool)
		// Phase 4 (§4): the per-provider rate limiter + a real jitter sleeper.
		Limiter: limiter,
		Sleep:   time.Sleep,
	}, fingerprint
}

// startHealthWatcher launches the §5 CONTINUOUS health-watcher in the background — the P4 control
// loop that REPLACES the P3 minimal re-audit ticker (§1.2). It owns the audit cadence (via the P3
// scheduler's Observe: fetch + record, no transition) AND adds liveness polling + block-rate-driven
// quarantine, with the three guards (sample gate GAP-27, hysteresis, fleet-wide brake GAP-28) and
// the canary recovery (GAP-30). It is the SINGLE WRITER of nodes.status (§5.1). The block-rate (A1)
// trigger is recommend-only by default (DR-2) — zero live block-rate mutation until an owner
// promotes A1 to auto. The client is the live HTTPClient (real /v1/audit + /healthcheck); on the
// local build with no real box the audit fetch fails → no promotion (honest, harden-toward-unknown).
// startRawPartitionMaintenance launches the ADR-0017 loop on ctx. It takes the narrow
// store.RawPartitioner (not *store.PG) so the wiring is testable without a database, and it reads N and
// the cadence from the config dials — nothing is hardcoded here.
func startRawPartitionMaintenance(ctx context.Context, cfg config.Config, rp store.RawPartitioner) {
	log.Printf("raw-partitions: maintenance starting (keep %d future month(s); re-check every %s)",
		cfg.Dials.RawPartitionMonthsAhead, cfg.RawPartitionMaintainInterval())
	go store.RunRawPartitionMaintenance(ctx, rp, cfg.Dials.RawPartitionMonthsAhead, cfg.RawPartitionMaintainInterval(), log.Printf)
}

func startHealthWatcher(ctx context.Context, cfg config.Config, client *node.HTTPClient, pg *store.PG, tokenFor func(string) string) {
	pool := make([]health.WatchedNode, 0, len(cfg.Nodes))
	for _, nd := range cfg.Nodes {
		if !nd.Enabled {
			continue
		}
		pool = append(pool, health.WatchedNode{
			NodeRef:    audit.NodeRef{ID: nd.ID, BaseURL: nd.BaseURL, Token: tokenFor(nd.ID), PublicEgressIP: nd.PublicEgressIP},
			VerifierIP: nd.PublicEgressIP, // == the node's egress IP (the block-rate GROUP BY key, §5.6)
		})
	}
	if len(pool) == 0 {
		return
	}
	sched := audit.NewScheduler(audit.Deps{Client: client, Store: pg}, cfg.Dials.CanaryRecoverGreens)
	blockMode := health.ModeRecommendOnly // DR-2: A1 statistical trigger defaults to recommend-only
	if cfg.ShadowModes.A1BlockRateQuarantine == config.ModeAuto {
		blockMode = health.ModeAuto // owner-promoted (admin-audited under DR-3)
	}
	params := health.Params{
		MinSamples:        cfg.Dials.BlockMinSamples,
		QuarThreshold:     float64(cfg.Dials.BlockQuarThresholdPct) / 100.0,
		RecoverThreshold:  float64(cfg.Dials.BlockRecoverThreshPct) / 100.0,
		RecoverGreens:     cfg.Dials.CanaryRecoverGreens,
		MinCanarySamples:  cfg.Dials.CanaryMinSamples, // §5.4/GAP-30 (no zero-sample promotion)
		LivenessMisses:    cfg.Dials.LivenessMisses,
		FleetWideFraction: float64(cfg.Dials.FleetWidePct) / 100.0,
		MinActiveFloor:    cfg.Dials.PoolEligibleFloor, // §5.5 backstop: block-rate never drops below the serving floor
		BlockMode:         blockMode,
	}
	w := health.NewWatcher(health.Deps{
		Audit: sched, Liveness: client, BlockRate: pg, Status: pg,
	}, params, cfg.BlockRateWindow(), cfg.Dials.CanaryWarmupCap)
	log.Printf("health-watcher: starting (cadence=%s window=%s block_mode=%s fleet_brake=%d%%)",
		cfg.WatcherCadence(), cfg.BlockRateWindow(), blockMode, cfg.Dials.FleetWidePct)
	go w.Run(ctx, pool, cfg.WatcherCadence())
}
