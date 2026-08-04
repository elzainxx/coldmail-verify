package queue

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"time"

	"github.com/coldmail-verify/control-plane/internal/hash"
	"github.com/coldmail-verify/control-plane/internal/idgen"
	"github.com/coldmail-verify/control-plane/internal/store"
)

// DeriveIdempotencyKey computes the work-item dedup / settle-once key (§5.1):
//
//	sha256(email_hash ‖ policy_key ‖ policy_version ‖ config_fingerprint)
//
// It REUSES the Door-3 artifact (email_hash — already HMAC'd under the pepper), so there is NO new
// hashing scheme and NO new pepper exposure (Door 3). It is a plain SHA-256 lookup digest, NOT an
// HMAC — the secret (pepper) already lives inside email_hash; re-HMACing would be redundant. Same
// (address × policy × config) ⇒ same key (dedup, §5.5); a config change ⇒ a new key ⇒ a new logical
// check, exactly as the cache keys on config_fingerprint (a config change should re-verify).
func DeriveIdempotencyKey(emailHash []byte, policyKey string, policyVersion int, configFingerprint string) []byte {
	h := sha256.New()
	h.Write(emailHash)
	h.Write([]byte{0}) // domain separator so field boundaries can't collide
	h.Write([]byte(policyKey))
	h.Write([]byte{0})
	var vb [8]byte
	binary.BigEndian.PutUint64(vb[:], uint64(policyVersion))
	h.Write(vb[:])
	h.Write([]byte(configFingerprint))
	return h.Sum(nil)
}

// Enqueuer turns a bulk submission into durable work-items (§7.1). It is the ONLY place plaintext
// enters the queue: it normalizes + hashes each address (Door 3), seals the plaintext under the
// DR-4 cipher (AAD = the new row id), derives the idempotency key, and enqueues. Cache/dedup
// short-circuits happen at DRAIN time (compute()'s cache lookup + the store's idempotency_key
// dedup), so the enqueuer stays simple and the results are uniform /v1/verify objects.
//
// GAP-36 boundary: the Enqueuer knows addresses/domains/policy + an OPAQUE caller_ref only. It does
// NOT parse CSVs, count credits, or track per-row UI — that is the platform's job. Malformed input
// is COUNTED (rejected) and reported, never a work-item (a bulk item must be a normalizable address).
type Enqueuer struct {
	Store             store.Store
	Hasher            *hash.Hasher
	Cipher            *AddrCipher
	ConfigFingerprint string
	Now               func() time.Time
}

// BulkResult is what SubmitBulk returns: the job handle + counts (§7.1). Malformed is the count of
// inputs that didn't normalize (reported so the caller isn't silently short a few rows); Enqueued
// is the number of durable work-items created (post dedup-within-this-submission).
type BulkResult struct {
	JobID     string
	Total     int // normalizable addresses accepted into the job
	Enqueued  int // work-items actually created (duplicates within the batch collapse)
	Duplicate int // inputs that deduped to an already-present idempotency_key
	Malformed int // inputs that failed to normalize (Door 3) — rejected, not queued
}

// SubmitBulk creates a job and enqueues one work-item per NORMALIZABLE address (§7.1). It returns
// immediately with the job handle — no SMTP runs here (the drainer does that later through the P4
// gates). policyKey/policyVersion/configFingerprint govern every item (part of the idempotency
// key). callerRef is the opaque platform correlation string (§7.6), echoed, never interpreted.
func (e *Enqueuer) SubmitBulk(ctx context.Context, addresses []string, policyKey string, policyVersion int, callerRef string) (BulkResult, error) {
	now := e.now()
	jobID, err := idgen.NewV7String(now)
	if err != nil {
		return BulkResult{}, err
	}

	// Normalize + build the work-items first so total_count reflects the REAL accepted set.
	type pending struct {
		item store.WorkItemRow
	}
	var items []pending
	res := BulkResult{JobID: jobID}
	for _, raw := range addresses {
		normalized, domain, nerr := e.Hasher.Normalize(raw)
		if nerr != nil {
			res.Malformed++ // Door-3 un-normalizable → rejected, never a work-item (§7.6 boundary)
			continue
		}
		emailHash := e.Hasher.EmailHash(normalized)
		itemID, ierr := idgen.NewV7String(now)
		if ierr != nil {
			return BulkResult{}, ierr
		}
		// DR-4: seal the plaintext under the row id (AAD) — anti-replay. The plaintext exists here
		// only transiently; the row stores ciphertext, purged on settle.
		sealed, serr := e.Cipher.Seal(normalized, []byte(itemID))
		if serr != nil {
			return BulkResult{}, serr
		}
		items = append(items, pending{item: store.WorkItemRow{
			ID:                itemID,
			IdempotencyKey:    DeriveIdempotencyKey(emailHash, policyKey, policyVersion, e.ConfigFingerprint),
			JobID:             jobID,
			Domain:            domain,
			EmailHash:         emailHash,
			PolicyKey:         policyKey,
			PolicyVersion:     policyVersion,
			ConfigFingerprint: e.ConfigFingerprint,
			Status:            "ready",
			NotBefore:         now,
			CreatedAt:         now,
			AddrCiphertext:    sealed,
		}})
	}
	res.Total = len(items)

	// Create the job with the accepted total, then enqueue each item. Enqueue dedups on
	// idempotency_key (§5.5): a duplicate within this submission (or a still-pending identical check
	// from another job) collapses — counted as Duplicate, not double-verified.
	if _, err := e.Store.CreateJob(ctx, store.JobRow{
		ID: jobID, PolicyKey: policyKey, PolicyVersion: policyVersion, Status: "running",
		TotalCount: res.Total, CallerRef: callerRef,
	}); err != nil {
		return BulkResult{}, err
	}
	for _, p := range items {
		_, existed, eerr := e.Store.EnqueueWorkItem(ctx, p.item)
		if eerr != nil {
			return BulkResult{}, eerr
		}
		if existed {
			res.Duplicate++
		} else {
			res.Enqueued++
		}
	}
	return res, nil
}

func (e *Enqueuer) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}
