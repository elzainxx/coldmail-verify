package flywheel

import "context"

// This file: the OutcomeSource ADAPTERS (ADR-0013 §B.3). The transport decision (mutually-authenticated
// pull) lives ENTIRELY here, behind the interface — the ingestor, classifier, and calibration never see
// it. Switching to signed webhooks later is a NEW adapter in this file, zero change elsewhere (the
// Door-5 "interfaces at the seams" payoff). Two adapters ship:
//
//   - SimulatedSource — the LOCAL-BUILD stand-in (authenticated by construction: it is in-process test
//     data). It makes the entire flywheel buildable + fail-verifiable NOW, before the sender exists.
//     A test flips Authed=false to prove the fail-closed property (unauthenticated ⇒ nothing ingested).
//
//   - PullSource — the SHAPE of the DECIDED mechanism (mutually-authenticated pull / mTLS). This phase
//     ships it as a PENDING-REAL-SENDER skeleton: Authenticated() is true only when a real mTLS client
//     is wired (Client != nil). With no client it is fail-closed (Authenticated()=false ⇒ the ingestor
//     pulls nothing), the honest "the sender does not exist yet" posture — never a fake authenticated
//     feed. The real HTTP/mTLS client slots into Client when the sender ("Feature 2") is built.

// SimulatedSource is an in-memory, authenticated-by-construction outcome feed for local build + tests
// (ADR-0013 §B.3). Outcomes are appended by the test; Pull returns them after the given cursor (the
// cursor is the index into the appended slice, as a string). Authed defaults true; a test sets it
// false to exercise the fail-closed ingestion path (the unauthenticated-outcome-never-calibrates test).
type SimulatedSource struct {
	Key      string       // the source key (watermark identity); defaults to "sim" if empty
	Authed   bool         // whether the source is "authenticated" — a test flips this to prove fail-closed
	Outcomes []RawOutcome // the feed contents (append-only; Pull reads a window after the cursor)
}

// NewSimulatedSource returns an authenticated simulated feed (the local-build default).
func NewSimulatedSource(key string) *SimulatedSource {
	if key == "" {
		key = "sim"
	}
	return &SimulatedSource{Key: key, Authed: true}
}

// Add appends an outcome to the simulated feed (test-support).
func (s *SimulatedSource) Add(o RawOutcome) { s.Outcomes = append(s.Outcomes, o) }

func (s *SimulatedSource) Authenticated() bool { return s.Authed }
func (s *SimulatedSource) SourceKey() string {
	if s.Key == "" {
		return "sim"
	}
	return s.Key
}

// Pull returns outcomes after `cursor` (a decimal index into Outcomes) up to `limit`, and the next
// cursor. Cursor "" starts at index 0. This mirrors a sender-owned append feed's cursor semantics
// (ADR-0013 §B.2): monotonic, replay-safe (re-reading from an old cursor returns the same window).
func (s *SimulatedSource) Pull(_ context.Context, cursor string, limit int) ([]RawOutcome, string, error) {
	start := parseCursor(cursor)
	if start < 0 || start > len(s.Outcomes) {
		start = len(s.Outcomes)
	}
	end := len(s.Outcomes)
	if limit > 0 && start+limit < end {
		end = start + limit
	}
	batch := append([]RawOutcome(nil), s.Outcomes[start:end]...)
	return batch, itoaCursor(end), nil
}

// MTLSClient is the (future) mutually-authenticated transport a PullSource uses (ADR-0013 §B.2). It is
// an interface so the real HTTP/mTLS client slots in when the sender exists — the flywheel never
// depends on a concrete transport (Door 5). PENDING-REAL-SENDER: no concrete client ships this phase.
type MTLSClient interface {
	// PullOutcomes fetches outcomes after `cursor` over the mutually-authenticated channel. A transport
	// / auth error is returned (the ingestor then ingests nothing this cycle — never fabricates).
	PullOutcomes(ctx context.Context, cursor string, limit int) (batch []RawOutcome, nextCursor string, err error)
}

// PullSource is the DECIDED mechanism's adapter (mutually-authenticated pull, ADR-0013 §B). It is
// FAIL-CLOSED: Authenticated() is true ONLY when a real mTLS Client is wired. With Client==nil (this
// phase — the sender doesn't exist) it authenticates as false, so the ingestor pulls nothing (the
// honest PENDING-REAL-SENDER posture — never a fake authenticated feed). This is the P7 analogue of
// P6's buildExternal returning nil (PENDING-REAL-VENDOR): the mechanism is built + wired, only the real
// counterparty is deferred.
type PullSource struct {
	Key    string
	Client MTLSClient // nil this phase (PENDING-REAL-SENDER) → Authenticated()=false → fail-closed
}

func (p *PullSource) Authenticated() bool { return p.Client != nil }
func (p *PullSource) SourceKey() string {
	if p.Key == "" {
		return "mtls-pull"
	}
	return p.Key
}

func (p *PullSource) Pull(ctx context.Context, cursor string, limit int) ([]RawOutcome, string, error) {
	if p.Client == nil {
		// PENDING-REAL-SENDER: no mTLS client → nothing to pull (fail-closed; IngestOnce also guards on
		// Authenticated()==false before reaching here, so this is a defensive backstop).
		return nil, cursor, nil
	}
	return p.Client.PullOutcomes(ctx, cursor, limit)
}

// ── tiny cursor helpers (decimal index; no strconv import to keep the adapter dependency-free) ──

func parseCursor(s string) int {
	if s == "" {
		return 0
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return -1
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func itoaCursor(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
