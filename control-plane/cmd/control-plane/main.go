// Command control-plane is the control-plane entrypoint. Phase 0: load config + secrets, wire
// the hasher and the HTTP front door, and serve. It builds and runs empty — no real
// verification. It proves: config loads (manifest flags readable), the secrets scaffold
// works, the kill-switch defaults to cheap-layers-only, and the Door-1 endpoint responds.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mnfi/codmailling/control-plane/internal/api"
	"github.com/mnfi/codmailling/control-plane/internal/config"
	"github.com/mnfi/codmailling/control-plane/internal/hash"
	"github.com/mnfi/codmailling/control-plane/internal/orchestrator"
	"github.com/mnfi/codmailling/control-plane/internal/policy"
	"github.com/mnfi/codmailling/control-plane/internal/secrets"
	"github.com/mnfi/codmailling/control-plane/internal/store"
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

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           buildAPI(cfg, sec, hasher).Handler(),
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
func buildAPI(cfg config.Config, sec *secrets.Secrets, hasher *hash.Hasher) *api.Server {
	srv := api.New(cfg, sec, hasher)
	if hasher == nil || cfg.DatabaseURL == "" {
		log.Printf("verify: no %s configured or no pepper — /v1/verify will report not-ready (503)",
			"TM_DATABASE_URL")
		return srv
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pg, err := store.OpenPG(ctx, cfg.DatabaseURL)
	if err != nil {
		// A configured-but-unreachable DB is a real error, but the control-plane can still
		// serve health/readiness; verify stays 503 until the DB is reachable. Log loudly.
		log.Printf("verify: store unavailable (%v) — /v1/verify will report not-ready (503)", err)
		return srv
	}
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
	orch := orchestrator.New(orchestrator.Deps{
		Hasher:            hasher,
		Store:             pg,
		CacheTTL:          cfg.CacheTTL(),
		ConfigFingerprint: cfg.ConfigFingerprint(),
	})
	log.Printf("verify: orchestrator wired (cheap-layers-only, policy=%s v%d, cache_ttl=%s)",
		def.Key, def.Version, cfg.CacheTTL())
	return srv.WithVerifier(orch, def)
}
