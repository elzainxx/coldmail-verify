package store

// mem_idem.go — the in-memory counterparts of postgres_idem.go and postgres_nodes.go (migration 0019): idempotent jobs, the one-read job status
// snapshot and node self-promotion. Each mirrors what Postgres enforces, INCLUDING the constraints it enforces (a fake laxer than the real store is
// how a defect stays invisible to the suite), and holds m.mu for the whole of each operation, which is the in-memory form of "one transaction" and
// "one statement".

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/coldmail-verify/control-plane/internal/vocab"
)

// keyedJob is what the unique index on (idem_caller_id, idempotency_key) holds: the job the key created and the fingerprint of its request.
type keyedJob struct {
	jobID       string
	fingerprint []byte
}

// RecordKeyedJob: see Store.RecordKeyedJob. ALL-OR-NOTHING: every check (the claim, the links between rows, rejects and work items, everything
// RecordJob refuses, a work-item key that is already taken in the job) runs before anything is stored.
func (m *MemStore) RecordKeyedJob(_ context.Context, rec KeyedJobRecord, malformedListCap int) (KeyedJobOutcome, error) {
	if err := checkKeyedRecord(rec); err != nil {
		return KeyedJobOutcome{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := rec.Claim.CallerID + "\x00" + rec.Claim.Key
	if held, taken := m.keyedJobs[k]; taken {
		if !bytes.Equal(held.fingerprint, rec.Claim.Fingerprint) {
			return KeyedJobOutcome{Conflict: true}, nil
		}
		return KeyedJobOutcome{JobID: held.jobID, Replayed: true, Submission: m.submissionLocked(held.jobID, malformedListCap)}, nil
	}
	seen := make(map[string]bool, len(rec.Items))
	for _, it := range rec.Items {
		ik := it.JobID + "\x00" + hex.EncodeToString(it.IdempotencyKey)
		if _, taken := m.idemKeys[ik]; taken || seen[ik] {
			return KeyedJobOutcome{}, fmt.Errorf("store: record keyed job: work item %s repeats an idempotency key already present in job %s (uq_work_items_job_idem)", it.ID, it.JobID)
		}
		if _, exists := m.workItems[it.ID]; exists {
			return KeyedJobOutcome{}, fmt.Errorf("store: record keyed job: work item %s already exists (work_items_pkey)", it.ID)
		}
		seen[ik] = true
	}
	if err := m.recordJobLocked(rec.Job, rec.Rows, rec.Rejects); err != nil {
		return KeyedJobOutcome{}, err
	}
	for _, it := range rec.Items {
		if it.Status == "" {
			it.Status = "ready"
		}
		if it.CreatedAt.IsZero() {
			it.CreatedAt = it.NotBefore
		}
		m.workItems[it.ID] = it
		m.idemKeys[it.JobID+"\x00"+hex.EncodeToString(it.IdempotencyKey)] = it.ID
	}
	m.keyedJobs[k] = keyedJob{jobID: rec.Job.ID, fingerprint: append([]byte(nil), rec.Claim.Fingerprint...)}
	return KeyedJobOutcome{JobID: rec.Job.ID}, nil
}

// submissionLocked reproduces what a job's 202 said, from its rows. m.mu must be held.
func (m *MemStore) submissionLocked(jobID string, malformedListCap int) JobSubmission {
	rows := append([]JobRowEntry(nil), m.jobRows[jobID]...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].Ordinal < rows[j].Ordinal })
	sub := JobSubmission{Total: len(rows), MalformedRows: []MalformedRowRef{}}
	for _, r := range rows {
		if r.Malformed {
			sub.Malformed++
			if len(sub.MalformedRows) < malformedListCap {
				sub.MalformedRows = append(sub.MalformedRows, MalformedRowRef{Ordinal: r.Ordinal, ItemID: r.ItemID})
			}
		}
		if r.DuplicateOf != nil {
			sub.Duplicate++
		}
	}
	sub.Enqueued = sub.Total - sub.Duplicate - sub.Malformed
	sub.MalformedRowsTruncated = sub.Malformed > malformedListCap
	return sub
}

// JobStatusSnapshot: see Store.JobStatusSnapshot. One lock acquisition = one consistent read. A row is SETTLED exactly when the results listing
// would serve it (see ListJobResults): malformed, or its work item is settled|dead and carries its verdict.
func (m *MemStore) JobStatusSnapshot(_ context.Context, jobID, ownerUserID string) (JobSnapshot, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobOwnedBy(jobID, ownerUserID)
	if !ok {
		return JobSnapshot{}, false, nil
	}
	complete := true
	for _, w := range m.workItems {
		if w.JobID == jobID && w.Status != "settled" && w.Status != "dead" {
			complete = false
			break
		}
	}
	logByID := map[string]VerificationRow{}
	for _, r := range m.Log {
		logByID[r.ID] = r
	}
	snap := JobSnapshot{Job: j, Complete: complete}
	snap.Totals.ByStatus = map[string]int{
		string(vocab.StatusValid): 0, string(vocab.StatusInvalid): 0, string(vocab.StatusRisky): 0, string(vocab.StatusUnknown): 0,
	}
	for _, r := range m.jobRows[jobID] {
		snap.Totals.Submitted++
		if r.Malformed {
			snap.Totals.Malformed++
		}
		if r.DuplicateOf != nil {
			snap.Totals.Duplicates++
		}
		var verdictID string
		if r.Malformed {
			verdictID = r.VerificationID
		} else if w, found := m.workItems[r.WorkItemID]; found && (w.Status == "settled" || w.Status == "dead") && w.VerificationID != "" {
			verdictID = w.VerificationID
		} else {
			continue // not settled yet
		}
		snap.Totals.Settled++
		if v, found := logByID[verdictID]; found {
			if _, known := snap.Totals.ByStatus[v.Status]; known {
				snap.Totals.ByStatus[v.Status]++
			}
		}
	}
	snap.Totals.Pending = snap.Totals.Submitted - snap.Totals.Settled
	return snap, true, nil
}

// RegisterNode: see Store.RegisterNode.
func (m *MemStore) RegisterNode(_ context.Context, nodeID, _ /*baseURL*/ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	m.regSeq[nodeID] = m.seq
	m.registered[nodeID] = struct{}{}
	h := m.health[nodeID]
	h.NodeID = nodeID
	h.Status = "provisioning"
	h.Port25Open = false
	h.PromotedByAuditID = ""
	m.health[nodeID] = h
	return nil
}

// SettleProvisioningNode: see Store.SettleProvisioningNode. It mirrors promoteNodeSQL clause for clause.
func (m *MemStore) SettleProvisioningNode(_ context.Context, nodeID, auditID string, promote bool) (bool, error) {
	if auditID == "" {
		return false, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	h, registered := m.health[nodeID]
	if _, ok := m.registered[nodeID]; !ok || !registered || h.Status != "provisioning" {
		return false, nil
	}
	if !promote {
		h.Port25Open = false
		m.health[nodeID] = h
		return false, nil
	}
	regAt, hasReg := m.regSeq[nodeID]
	auditAt, hasAudit := m.auditSeq[auditID]
	if !hasReg || !hasAudit || auditAt <= regAt {
		return false, nil // registered_at IS NOT NULL AND a.created_at > n.registered_at
	}
	var row *NodeAuditRow
	for i := range m.NodeAudits {
		if m.NodeAudits[i].ID == auditID && m.NodeAudits[i].NodeID == nodeID {
			row = &m.NodeAudits[i]
			break
		}
	}
	if row == nil || row.AuditStatus != "green" {
		return false, nil
	}
	h.Status = "active"
	h.Port25Open = row.AuditStatus == "green" // taken from the audit row
	h.PromotedByAuditID = row.ID
	m.health[nodeID] = h
	return true, nil
}

// AllWorkItems returns every work item the store holds, in no particular order (a test aid, like WorkItemsForJob: it lets a test say "nothing but the
// original job's work items exists" after a replay).
func (m *MemStore) AllWorkItems() []WorkItemRow {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]WorkItemRow, 0, len(m.workItems))
	for _, w := range m.workItems {
		out = append(out, w)
	}
	return out
}
