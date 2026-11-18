package queue

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"time"

	hashpkg "github.com/coldmail-verify/control-plane/internal/hash"
	"github.com/coldmail-verify/control-plane/internal/idgen"
	"github.com/coldmail-verify/control-plane/internal/policy"
	"github.com/coldmail-verify/control-plane/internal/store"
	"github.com/coldmail-verify/control-plane/internal/verdict"
	"github.com/coldmail-verify/control-plane/internal/vocab"
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
// DR-4 cipher (AAD = the new row id), derives the idempotency key, and enqueues. Identical addresses
// WITHIN ONE JOB collapse here (one work item, every row shares its result); the cache short-circuit
// happens at DRAIN time (compute()'s cache lookup), so the enqueuer stays simple and the results are
// uniform /v1/verify objects. Nothing is deduplicated ACROSS jobs or tenants (0018, owner ruling A2).
//
// GAP-36 boundary: the Enqueuer knows addresses/domains/policy + an OPAQUE caller_ref only (and the
// caller's own per-row item_id label, which it stores and echoes without interpreting). It does NOT
// parse CSVs, count credits, or track per-row UI — that is the platform's job. Malformed input is
// COUNTED and reported with its ordinal, never a work-item (a work item must be a normalizable address).
type Enqueuer struct {
	Store             store.Store
	Hasher            *hashpkg.Hasher
	Cipher            *AddrCipher
	ConfigFingerprint string
	Now               func() time.Time
}

// BulkRow is one INPUT ROW of a bulk submission (A1): the address and the caller's optional label for
// the row. ItemID "" means the row has none (the `addresses` request shape). The API has already
// validated the labels (non-blank, <= store.MaxItemIDBytes, no control characters, unique in the
// request); the Enqueuer does not re-judge them, the database CHECKs and unique index are the backstop.
type BulkRow struct {
	ItemID  string
	Address string
}

// MalformedRowsCap bounds how many malformed rows a BulkResult (and so the create response) lists. The
// COUNT (BulkResult.Malformed) is always exact; only the list is capped, with MalformedRowsTruncated set.
const MalformedRowsCap = 1000

// MalformedRow names one input row whose address did not normalise: the caller's own index and label.
type MalformedRow struct {
	Ordinal int
	ItemID  string // "" => the row had none
}

// BulkResult is what SubmitBulk returns: the job handle + counts (§7.1). With A1/A2 (0018) the counts
// are over INPUT ROWS and add up: Total = Enqueued + Duplicate + Malformed.
type BulkResult struct {
	JobID     string
	Total     int // input rows, malformed and duplicate rows included
	Enqueued  int // work-items actually created: the first row of each distinct address in the job
	Duplicate int // rows that shared an EARLIER row's work-item in THIS job (never across jobs)
	Malformed int // rows that failed to normalize (Door 3): no work-item, reported as invalid_syntax

	// MalformedRows lists the malformed rows (at most MalformedRowsCap) so the caller can map them to its
	// own input; MalformedRowsTruncated is true when more than that many were malformed.
	MalformedRows          []MalformedRow
	MalformedRowsTruncated bool

	// Replayed (0019) is true when the submission carried an Idempotency-Key that already held a job of the SAME request: nothing was written, JobID
	// is the ORIGINAL job's id and every count above is what the original answer said. Always false for a submission without a key.
	Replayed bool
}

// IdemInput is the idempotency identity of a keyed submission (0019, owner ruling OD-6/OD-8): the AUTHENTICATED caller the key is scoped to
// (api_callers.caller_id, which the API takes from the credential, never from the body) and the caller's Idempotency-Key header, already validated
// (1..128 characters of [A-Za-z0-9._:-]).
type IdemInput struct {
	CallerID string
	Key      string
}

// ErrIdempotencyKeyReused is returned by SubmitBulkRowsKeyed when the key already holds a job of a DIFFERENT request. Nothing was created.
var ErrIdempotencyKeyReused = errors.New("queue: the idempotency key was already used for a different request")

// SubmitBulk is SubmitBulkRows for the `addresses` request shape: every address is a row with no
// item_id. Kept as the narrow entry point the original callers (and the end-to-end tests) use.
func (e *Enqueuer) SubmitBulk(ctx context.Context, addresses []string, def *policy.Definition, callerRef, ownerUserID string) (BulkResult, error) {
	rows := make([]BulkRow, len(addresses))
	for i, a := range addresses {
		rows[i] = BulkRow{Address: a}
	}
	return e.SubmitBulkRows(ctx, rows, def, callerRef, ownerUserID)
}

// preparedBulk is everything one submission computes BEFORE it writes anything: the job, its rows, the LOG rows of its malformed rows, its work
// items (plaintext already sealed), the counts the 202 reports and the fingerprint of the canonical request.
type preparedBulk struct {
	res         BulkResult
	job         store.JobRow
	entries     []store.JobRowEntry
	items       []store.WorkItemRow
	rejects     []store.VerificationRow
	fingerprint []byte
}

// writeFrame adds one length-prefixed field to the fingerprint, so no two different requests can produce the same byte stream.
func writeFrame(h hash.Hash, b []byte) {
	var n [binary.MaxVarintLen64]byte
	h.Write(n[:binary.PutUvarint(n[:], uint64(len(b)))])
	h.Write(b)
}

// prepareBulk is the body of every submission up to the first write: normalise + hash + seal each row (Door 3, DR-4), collapse duplicates inside the
// job, build the malformed rows' LOG rows, and fingerprint the canonical request. It writes nothing.
//
// THE FINGERPRINT (0019). sha256 over: a version tag; the EFFECTIVE policy key; the caller_ref; the owner; the row count; and per row, in order, its
// item_id, a kind byte and, for an addressable row, the KEYED email hash (HMAC under the pepper of the normalised address). So two requests are "the
// same request" iff they give the same rows in the same order after norm/v1: whitespace and key order of the JSON, the case of an address, an
// omitted policy against the default one do not matter; an address, a label, the caller_ref or the order do. A malformed row is identified by its
// position and label only (its content does not change what the job holds: an invalid_syntax row). The digest holds no address and no unkeyed hash
// of one (Door 3), and it excludes the config fingerprint and the policy VERSION on purpose: a retry after a deploy is still the same request.
func (e *Enqueuer) prepareBulk(rows []BulkRow, def *policy.Definition, callerRef, ownerUserID string) (preparedBulk, error) {
	if def == nil {
		return preparedBulk{}, fmt.Errorf("queue: a bulk submission needs the governing policy definition")
	}
	now := e.now()
	jobID, err := idgen.NewV7String(now)
	if err != nil {
		return preparedBulk{}, err
	}
	policyKey, policyVersion := def.Key, def.Version

	fp := sha256.New()
	writeFrame(fp, []byte("m1-bulk-request/v1"))
	writeFrame(fp, []byte(policyKey))
	writeFrame(fp, []byte(callerRef))
	writeFrame(fp, []byte(ownerUserID))
	var nrows [8]byte
	binary.BigEndian.PutUint64(nrows[:], uint64(len(rows)))
	writeFrame(fp, nrows[:])

	res := BulkResult{JobID: jobID, Total: len(rows)}
	entries := make([]store.JobRowEntry, 0, len(rows))
	// firstByKey maps an idempotency key (raw bytes as a string key) to the work item that holds it IN THIS
	// JOB and the ordinal of the first row that created it. It lives for one submission, which is exactly
	// the dedup scope: nothing is remembered across jobs, so a second job (any owner) finds it empty.
	type firstRow struct {
		itemID  string
		ordinal int
	}
	firstByKey := make(map[string]firstRow, len(rows))
	var items []store.WorkItemRow
	// rejects are the LOG rows of the malformed rows. Every one is the same verdict, built ONCE by the
	// assembler the sync path uses (rejectTemplate) and stamped with its own id; the maps inside are shared
	// and never written again, so a 100,000-row submission of garbage costs one set of maps, not 100,000.
	var rejects []store.VerificationRow
	var rejectTmpl *store.VerificationRow
	for ordinal, row := range rows {
		normalized, domain, nerr := e.Hasher.Normalize(row.Address)
		writeFrame(fp, []byte(row.ItemID))
		if nerr != nil {
			fp.Write([]byte{'m'})
			// Door-3 un-normalizable → no work-item (§7.6 boundary), but the row is kept: every input
			// row still has an outcome, its ordinal is the caller's own index, and the outcome is LOGGED.
			res.Malformed++
			if len(res.MalformedRows) < MalformedRowsCap {
				res.MalformedRows = append(res.MalformedRows, MalformedRow{Ordinal: ordinal, ItemID: row.ItemID})
			} else {
				res.MalformedRowsTruncated = true
			}
			if rejectTmpl == nil {
				t := e.rejectTemplate(def, now)
				rejectTmpl = &t
			}
			logRow := *rejectTmpl
			logID, ierr := idgen.NewV7String(now)
			if ierr != nil {
				return preparedBulk{}, ierr
			}
			reqID, ierr := idgen.NewV7String(now)
			if ierr != nil {
				return preparedBulk{}, ierr
			}
			logRow.ID, logRow.RequestID = logID, reqID
			rejects = append(rejects, logRow)
			entries = append(entries, store.JobRowEntry{Ordinal: ordinal, ItemID: row.ItemID, Malformed: true, VerificationID: logID})
			continue
		}
		emailHash := e.Hasher.EmailHash(normalized)
		fp.Write([]byte{'a'})
		fp.Write(emailHash)
		key := DeriveIdempotencyKey(emailHash, policyKey, policyVersion, e.ConfigFingerprint)
		if first, dup := firstByKey[string(key)]; dup {
			// The same address earlier in this job: no second check, the row shares the first one's result
			// and names the first row, so the caller can see these rows are ONE check.
			res.Duplicate++
			firstOrdinal := first.ordinal
			entries = append(entries, store.JobRowEntry{Ordinal: ordinal, ItemID: row.ItemID, WorkItemID: first.itemID, DuplicateOf: &firstOrdinal})
			continue
		}
		itemID, ierr := idgen.NewV7String(now)
		if ierr != nil {
			return preparedBulk{}, ierr
		}
		// DR-4: seal the plaintext under the row id (AAD) — anti-replay. The plaintext exists here
		// only transiently; the row stores ciphertext, purged on settle.
		sealed, serr := e.Cipher.Seal(normalized, []byte(itemID))
		if serr != nil {
			return preparedBulk{}, serr
		}
		firstByKey[string(key)] = firstRow{itemID: itemID, ordinal: ordinal}
		items = append(items, store.WorkItemRow{
			ID:                itemID,
			IdempotencyKey:    key,
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
		})
		entries = append(entries, store.JobRowEntry{Ordinal: ordinal, ItemID: row.ItemID, WorkItemID: itemID})
	}
	res.Enqueued = len(items)

	return preparedBulk{
		res: res,
		// The job's total is its INPUT ROWS (malformed and duplicates included), so total = enqueued +
		// duplicate + malformed and the status counts can reach pending 0.
		job: store.JobRow{
			ID: jobID, PolicyKey: policyKey, PolicyVersion: policyVersion, Status: string(vocab.JobRunning),
			TotalCount: res.Total, CallerRef: callerRef, OwnerUserID: ownerUserID,
		},
		entries: entries, items: items, rejects: rejects, fingerprint: fp.Sum(nil),
	}, nil
}

// SubmitBulkRows creates a job and records EVERY input row (§7.1; A1/A2, 0018). It returns immediately
// with the job handle — no SMTP runs here (the drainer does that later through the P4 gates).
// def is the governing policy: its key and version, with the config fingerprint, govern every item (part
// of the idempotency key), and its invalid_syntax baseline is the confidence of a malformed row's verdict.
// callerRef is the opaque platform correlation string (§7.6), echoed, never interpreted.
//
// What happens to each row, in submitted order (its index is its ordinal):
//   - an address that does not normalise is MALFORMED: it gets a job row, no work item, and its OWN LOG
//     row — the invalid/invalid_syntax verdict with a NULL hash that the sync path writes for such an
//     input (§3.2.7), logged here, before the 202, because Door 2 says every result is logged before it is
//     returned. Its result is served from that LOG row, with its ordinal and item_id. A repeated malformed
//     address is NOT a duplicate: each malformed row is its own row with its own LOG row;
//   - the FIRST row of a given (normalised address × policy × config) in this job creates the work item;
//   - a later identical row in the SAME job creates nothing, points at that work item and names the first
//     row (DuplicateOf), so the address is checked once, every row gets the result, and the caller can
//     see which rows are one check (counted as Duplicate).
//
// There is NO dedup across jobs or tenants: the idempotency key is unique per job (0018), so a new job
// always gets a work item for every distinct address, whatever any job before it did.
//
// ownerUserID is the PLATFORM user that will own the job (0013 / fix-plan item 1, D1) — the value
// every subsequent read predicates on. The two trailing string parameters are deliberately NOT
// interchangeable and the distinction is load-bearing: ownerUserID comes from the AUTHENTICATED
// KEY (api_callers.user_id, via LookupCaller) and is trusted; callerRef comes from the REQUEST BODY
// and is not. Swapping them would let a caller name its own owner — i.e. reintroduce D1 through the
// front door. They sit next to each other here, so this is exactly where that mistake would be made.
//
// "" is accepted rather than rejected (a key with no recorded platform user can still submit), and
// it stores a NULL owner, which makes the resulting job unreadable over the API by anyone. Failing
// CLOSED on read beats failing closed on submit: the alternative — rejecting the submission — would
// break the legacy/dev path for no security gain, since an unreadable job leaks nothing.
//
// ORDER OF WRITES: ONE transaction records the job, ALL its job rows and the LOG rows of its malformed
// rows (Store.RecordJob), then the work items are enqueued. A work item can therefore never settle before
// the rows that point at it exist, so the status counts (rows whose work item is terminal) are exact at
// every instant, and the malformed rows' verdicts are logged and committed before this returns, i.e.
// before the 202. (A failure part-way through the work items leaves a job the caller never learns the id
// of, as it always could.)
//
// THIS IS THE UNKEYED PATH and it is unchanged by 0019: a submission without an Idempotency-Key never touches the idempotency columns and writes
// exactly what it wrote before. See SubmitBulkRowsKeyed for the keyed one.
func (e *Enqueuer) SubmitBulkRows(ctx context.Context, rows []BulkRow, def *policy.Definition, callerRef, ownerUserID string) (BulkResult, error) {
	p, err := e.prepareBulk(rows, def, callerRef, ownerUserID)
	if err != nil {
		return BulkResult{}, err
	}
	// The job, its rows and the malformed rows' LOG rows commit together (or not at all) before anything is enqueued or returned.
	if err := e.Store.RecordJob(ctx, p.job, p.entries, p.rejects); err != nil {
		return BulkResult{}, err
	}
	for _, it := range p.items {
		// The key was de-duplicated above and the job id is brand new, so the store cannot already hold
		// it; if it reports otherwise, a job row would point at a work item that does not exist, so refuse
		// loudly rather than hand back a job whose results can never be served.
		_, existed, eerr := e.Store.EnqueueWorkItem(ctx, it)
		if eerr != nil {
			return BulkResult{}, eerr
		}
		if existed {
			return BulkResult{}, fmt.Errorf("queue: work item key already present in the brand-new job %s", p.job.ID)
		}
	}
	return p.res, nil
}

// SubmitBulkRowsKeyed is SubmitBulkRows for a request that carries an Idempotency-Key (0019, owner ruling OD-6/OD-8), scoped to idem.CallerID:
//
//   - a NEW key creates the job exactly as SubmitBulkRows would, except that the job, its rows, the malformed rows' LOG rows AND its work items are
//     written in ONE transaction (Store.RecordKeyedJob), so the job is never visible half-made and a repeat of the key can never meet a job whose
//     work items do not exist yet;
//   - a key that already holds a job of the SAME request (same fingerprint) creates NOTHING and returns that job: the same JobID and the counts of
//     the original answer, with Replayed set. The rows prepared for this attempt are discarded unwritten, plaintext included;
//   - a key that holds a job of ANOTHER request creates nothing and returns ErrIdempotencyKeyReused;
//   - two requests with one key at the same instant create exactly one job: the second waits for the first transaction (the unique index) and is
//     then a replay, or, if the first failed, creates the job itself.
func (e *Enqueuer) SubmitBulkRowsKeyed(ctx context.Context, rows []BulkRow, def *policy.Definition, callerRef, ownerUserID string, idem IdemInput) (BulkResult, error) {
	p, err := e.prepareBulk(rows, def, callerRef, ownerUserID)
	if err != nil {
		return BulkResult{}, err
	}
	out, err := e.Store.RecordKeyedJob(ctx, store.KeyedJobRecord{
		Job: p.job, Rows: p.entries, Rejects: p.rejects, Items: p.items,
		Claim: store.IdemClaim{CallerID: idem.CallerID, Key: idem.Key, Fingerprint: p.fingerprint},
	}, MalformedRowsCap)
	if err != nil {
		return BulkResult{}, err
	}
	switch {
	case out.Conflict:
		return BulkResult{}, ErrIdempotencyKeyReused
	case out.Replayed:
		res := BulkResult{
			JobID: out.JobID, Total: out.Submission.Total, Enqueued: out.Submission.Enqueued,
			Duplicate: out.Submission.Duplicate, Malformed: out.Submission.Malformed,
			MalformedRowsTruncated: out.Submission.MalformedRowsTruncated, Replayed: true,
		}
		for _, m := range out.Submission.MalformedRows {
			res.MalformedRows = append(res.MalformedRows, MalformedRow{Ordinal: m.Ordinal, ItemID: m.ItemID})
		}
		return res, nil
	}
	return p.res, nil
}

// RejectConfidence is the confidence of an un-addressable input's verdict: the policy's baseline for invalid_syntax, 99 when the policy
// names none. It is the SAME rule the sync path applies (orchestrator.confidenceForReject); that function is unexported, so the rule is
// restated here and TestSubmitBulkRows_AMalformedRowIsLoggedLikeTheSyncPathLogsIt compares a bulk row with a real sync Verify so the two
// cannot drift.
func RejectConfidence(def *policy.Definition) int {
	if v, ok := def.Confidence.BaselineByReason[string(vocab.ReasonInvalidSyntax)]; ok {
		return v
	}
	return 99
}

// rejectTemplate builds the LOG row of an un-addressable input exactly as the sync path does (orchestrator.computeReject, logged by
// logOnly): invalid / invalid_syntax / skip, the policy's invalid_syntax confidence, signals {"syntax_valid": false}, from_cache false,
// NO email_hash and NO hash_scheme (§3.2.7), no domain. The assembler (verdict.Assemble) is the one the sync path uses, so the response
// shape and the LOG row cannot drift. The caller stamps each copy with its own id and request id. RawEmail is deliberately left empty:
// the LOG never holds an address (Door 3), and the bulk result does not echo one.
func (e *Enqueuer) rejectTemplate(def *policy.Definition, now time.Time) store.VerificationRow {
	core := verdict.Core{
		Status: vocab.StatusInvalid, Reason: vocab.ReasonInvalidSyntax,
		Confidence: RejectConfidence(def), Recommendation: vocab.RecSkip,
		Flags: map[string]bool{}, Signals: map[string]any{"syntax_valid": false},
	}
	prov := verdict.Provenance{
		CheckedAt: now, PolicyKey: def.Key, PolicyVersion: def.Version, ConfigFingerprint: e.ConfigFingerprint,
		NormalizedDomain: "", EmailHash: nil, // NULL hash (§3.2.7)
	}
	_, row := verdict.Assemble(core, prov)
	return row
}

func (e *Enqueuer) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}
