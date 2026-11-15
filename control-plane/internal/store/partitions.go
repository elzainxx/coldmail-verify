package store

// partitions.go — keeping verification_raw's future monthly partitions created (ADR-0017).
//
// verification_raw is RANGE-partitioned by month on created_at, and a row dated in a month that has
// no partition is REJECTED (SQLSTATE 23514). Migration 0001 created three partitions "as examples"
// and nothing ever created another, so from 2026-10-01 every SMTP verdict's raw-sidecar insert — and
// therefore the verdict write, which is atomic with it (§6.4) — failed. This file is the missing
// half: the control-plane keeps N future months created (the TM_RAW_PARTITION_MONTHS_AHEAD dial).
//
// DDL stays off the runtime role (Door 2 / PN-9). The only door is the SECURITY DEFINER function
// ensure_verification_raw_partitions(n) from migration 0014, whose reference time is the database
// clock; this code just calls it.

import (
	"context"
	"fmt"
	"time"
)

// RawPartitioner is what the maintenance loop needs. *PG implements it; the in-memory store has no
// partitions and deliberately does not.
type RawPartitioner interface {
	// EnsureRawPartitions makes sure the partitions for this month .. monthsAhead months from now exist
	// and returns how many it had to create (0 on an idempotent re-run).
	EnsureRawPartitions(ctx context.Context, monthsAhead int) (created int, err error)
}

var _ RawPartitioner = (*PG)(nil)

// EnsureRawPartitions calls the runtime door from migration 0014.
func (p *PG) EnsureRawPartitions(ctx context.Context, monthsAhead int) (int, error) {
	var created int
	if err := p.pool.QueryRow(ctx, `SELECT verify.ensure_verification_raw_partitions($1)`, monthsAhead).Scan(&created); err != nil {
		return 0, fmt.Errorf("store: ensure raw partitions: %w", err)
	}
	return created, nil
}

// MaintainRawPartitionsOnce is ONE tick of the maintenance loop, and the single place a tick's logic
// lives (the loop and the tests both call it, so a test of this function is a test of the loop's body).
// monthsAhead < 1 is refused: 0 would mean "never any headroom", i.e. the cliff reappearing at every
// month boundary — the config loader rejects it too, but a library function must not trust that.
func MaintainRawPartitionsOnce(ctx context.Context, rp RawPartitioner, monthsAhead int) (int, error) {
	if monthsAhead < 1 {
		return 0, fmt.Errorf("store: raw partition maintenance needs monthsAhead >= 1 (got %d): 0 is the cliff", monthsAhead)
	}
	return rp.EnsureRawPartitions(ctx, monthsAhead)
}

// rawPartitionTickTimeout bounds one tick's DB work. A safety bound, not an operator dial: the call
// is a handful of catalog lookups plus at most N CREATE TABLEs.
const rawPartitionTickTimeout = 30 * time.Second

// RunRawPartitionMaintenance ticks immediately (so a process that starts after a long outage heals
// within seconds of startup; it does NOT block serving) and then every `every`, until ctx is done. A failed tick is logged
// and the loop continues — a transient database error must never take the control-plane down — but the
// log line states the consequence, because a maintenance loop that fails silently is exactly how the
// cliff happened.
func RunRawPartitionMaintenance(ctx context.Context, rp RawPartitioner, monthsAhead int, every time.Duration, logf func(format string, args ...any)) {
	tick := func() {
		tctx, cancel := context.WithTimeout(ctx, rawPartitionTickTimeout)
		defer cancel()
		created, err := MaintainRawPartitionsOnce(tctx, rp, monthsAhead)
		switch {
		case err != nil:
			logf("raw-partitions: maintenance FAILED (%v) — verification_raw will run out of partitions and SMTP verdict writes will fail once the last covered month ends; retrying in %s", err, every)
		case created > 0:
			logf("raw-partitions: created %d verification_raw partition(s); keeping %d future month(s) ahead", created, monthsAhead)
		}
	}
	tick()
	if every <= 0 {
		// time.NewTicker panics on a non-positive interval. The config loader rejects it, but this is a
		// library function: run the one tick above and say so, rather than crash the process or invent
		// a default for a value the operator owns.
		logf("raw-partitions: non-positive interval %s — ran ONE tick and will not repeat; fix TM_RAW_PARTITION_MAINTAIN_INTERVAL_SECONDS", every)
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}
