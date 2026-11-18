package store

// postgres_nodes.go — node SELF-PROMOTION on Postgres (migration 0019, owner ruling OD-4): the boot registration that never trusts what an earlier
// process left behind, and the single statement that promotes a node from ONE audit row. See Docs/install-and-deploy.md for the operator's view.

import (
	"context"
	"fmt"
)

// RegisterNode: see Store.RegisterNode. One statement: the row is created, or an existing one is reset to provisioning / port25_open=false with a
// fresh registered_at and no promoting audit. registered_at is the DATABASE's clock (now()), the same clock node_audits.created_at takes by default,
// so "recorded after the registration" never compares two machines' clocks.
func (p *PG) RegisterNode(ctx context.Context, nodeID, baseURL string) error {
	const q = `
INSERT INTO verify.nodes (id, base_url, status, port25_open, enabled, registered_at, promoted_by_audit_id)
VALUES ($1, $2, 'provisioning', false, true, now(), NULL)
ON CONFLICT (id) DO UPDATE SET
  base_url = EXCLUDED.base_url,
  status = 'provisioning',
  port25_open = false,
  registered_at = EXCLUDED.registered_at,
  promoted_by_audit_id = NULL`
	if _, err := p.pool.Exec(ctx, q, nodeID, baseURL); err != nil {
		return fmt.Errorf("store: register node: %w", err)
	}
	return nil
}

// promoteNodeSQL promotes a node in ONE statement from ONE audit row. Everything that makes the promotion legitimate is in the WHERE clause, so there
// is no window between checking and writing:
//   - the node is still provisioning (an active, quarantined or recovering node is never touched);
//   - the audit row is this node's, is green, and was recorded AFTER this boot's registration (a green row an earlier process recorded, however
//     recent, never promotes a node that has just been re-registered);
//   - port25_open is TAKEN FROM the audit row (true iff it is green), and the audit row's id is written beside the new status.
const promoteNodeSQL = `
UPDATE verify.nodes n
SET status = 'active', port25_open = (a.audit_status = 'green'), promoted_by_audit_id = a.id
FROM verify.node_audits a
WHERE n.id = $1 AND a.id = $2 AND a.node_id = n.id
  AND n.status = 'provisioning'
  AND n.registered_at IS NOT NULL AND a.created_at > n.registered_at
  AND a.audit_status = 'green'`

// SettleProvisioningNode: see Store.SettleProvisioningNode.
func (p *PG) SettleProvisioningNode(ctx context.Context, nodeID, auditID string, promote bool) (bool, error) {
	if auditID == "" {
		return false, nil
	}
	if promote {
		tag, err := p.pool.Exec(ctx, promoteNodeSQL, nodeID, auditID)
		if err != nil {
			return false, fmt.Errorf("store: promote node from audit: %w", err)
		}
		return tag.RowsAffected() == 1, nil
	}
	// Not promoted: the node stays provisioning and what the audit says about egress is "no": port25_open false (it may have been set by hand).
	if _, err := p.pool.Exec(ctx,
		`UPDATE verify.nodes SET port25_open = false WHERE id = $1 AND status = 'provisioning' AND port25_open IS DISTINCT FROM false`, nodeID); err != nil {
		return false, fmt.Errorf("store: settle provisioning node: %w", err)
	}
	return false, nil
}
