package market

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Tickets have no separate preservation-scope UI. Existing account holds cover
// the opener, current and historical participants, message senders, and staff
// named in the fixed workflow history. A role or assignment change must not
// silently detach evidence from an existing account preservation obligation.
const retentionTicketHeldSQL = `EXISTS(SELECT 1 FROM retention_holds h WHERE h.scope='account' AND h.released_at IS NULL AND (
 h.record_id IN(t.opener_id,t.assigned_to)
 OR EXISTS(SELECT 1 FROM ticket_participants hp WHERE hp.ticket_id=t.id AND hp.user_id=h.record_id)
 OR EXISTS(SELECT 1 FROM ticket_messages hm WHERE hm.ticket_id=t.id AND hm.sender_id=h.record_id)
 OR EXISTS(SELECT 1 FROM ticket_events he WHERE he.ticket_id=t.id AND h.record_id IN(he.actor_id,he.staff_id))))`

// Each category uses the same bounded, journaled tombstones as other private
// channels. Empty closed threads are selected on the pass after their messages
// expire; open ticket metadata never triggers an implicit workflow close.
func retentionTicketCandidates(ctx context.Context, tx *sql.Tx, now time.Time) ([]retentionTombstone, error) {
	var out []retentionTombstone
	for _, kind := range []string{"ticket_message", "ticket"} {
		var rows *sql.Rows
		var err error
		switch kind {
		case "ticket_message":
			rows, err = tx.QueryContext(ctx, `SELECT m.id FROM ticket_messages m JOIN tickets t ON t.id=m.ticket_id
		 WHERE (m.created<=$1::timestamptz-interval '90 days' OR m.sender_id IS NULL OR t.opener_id IS NULL
		 OR EXISTS(SELECT 1 FROM users u WHERE u.closed_at IS NOT NULL AND u.id IN(m.sender_id,t.opener_id)))
		 AND NOT `+retentionTicketHeldSQL+` ORDER BY m.created,m.id LIMIT 200 FOR UPDATE OF m,t`, now)
		case "ticket":
			rows, err = tx.QueryContext(ctx, `SELECT t.id FROM tickets t WHERE t.status IN('closed','ignored') AND t.updated<=$1::timestamptz-interval '90 days'
		 AND NOT EXISTS(SELECT 1 FROM ticket_messages m WHERE m.ticket_id=t.id)
		 AND NOT `+retentionTicketHeldSQL+` ORDER BY t.updated,t.id LIMIT 200 FOR UPDATE OF t`, now)
		}
		if err != nil {
			return nil, fmt.Errorf("retention candidates %s: %w", kind, err)
		}
		ids, err := retentionScanIDs(rows)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			out = append(out, retentionTombstone{Kind: kind, ID: id, At: now})
		}
	}
	return out, nil
}
