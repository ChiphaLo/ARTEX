package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

var ErrInboxConflict = errors.New("消息已改变或开始处理，请刷新队列")
var ErrInboxFull = errors.New("message queue is full (32 pending messages)")

type AgentInboxItem struct {
	ID             int64           `json:"id"`
	Scope          string          `json:"-"`
	TaskID         int64           `json:"-"`
	ConversationID int64           `json:"-"`
	MainSeg        int             `json:"-"`
	Mode           string          `json:"mode"`
	State          string          `json:"state"`
	Revision       int             `json:"revision"`
	Payload        json.RawMessage `json:"-"`
}

// All pending mutations/claims for a scope share this lock. A message cannot be
// edited/reordered after delivery has won the race. No lock spans an agent run.
func (d *DB) inboxTx(ctx context.Context, scope string) (*sql.Tx, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1)::bigint)`, scope); err != nil {
		tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func (d *DB) EnqueueAgentMessage(scope string, taskID, convID int64, seg int, requestID, mode string, payload []byte) (AgentInboxItem, error) {
	var item AgentInboxItem
	tx, err := d.inboxTx(context.Background(), scope)
	if err != nil {
		return item, err
	}
	defer tx.Rollback()
	var original []byte
	err = tx.QueryRow(`SELECT `+inboxColumns+`,original_payload FROM agent_inbox WHERE scope_key=$1 AND request_id=$2`, scope, requestID).Scan(&item.ID, &item.Scope, &item.TaskID, &item.ConversationID, &item.MainSeg, &item.Mode, &item.State, &item.Revision, &item.Payload, &original)
	if err == nil {
		var a, b any
		_ = json.Unmarshal(original, &a)
		_ = json.Unmarshal(payload, &b)
		// Mention expansion is a snapshot, not part of the user's idempotency
		// identity: referenced data may change while a lost response is retried.
		if m, ok := a.(map[string]any); ok {
			delete(m, "agent_message")
		}
		if m, ok := b.(map[string]any); ok {
			delete(m, "agent_message")
		}
		if item.Mode != mode || !reflect.DeepEqual(a, b) {
			return item, ErrInboxConflict
		}
		return item, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return item, err
	}
	var pending int
	if err := tx.QueryRow(`SELECT count(*) FROM agent_inbox WHERE scope_key=$1 AND state='pending'`, scope).Scan(&pending); err != nil {
		return item, err
	}
	if pending >= 32 {
		return item, ErrInboxFull
	}
	err = tx.QueryRow(`INSERT INTO agent_inbox(scope_key,task_id,conversation_id,main_seg,request_id,mode,payload,original_payload,position)
 VALUES ($1,NULLIF($2,0),NULLIF($3,0),$4,$5,$6,$7,$7,(SELECT COALESCE(max(position),0)+1 FROM agent_inbox WHERE scope_key=$1)) RETURNING id`, scope, taskID, convID, seg, requestID, mode, string(payload)).Scan(&item.ID)
	if err != nil {
		return item, err
	}
	item.Scope, item.TaskID, item.ConversationID, item.MainSeg, item.Mode, item.State, item.Revision, item.Payload = scope, taskID, convID, seg, mode, "pending", 1, payload
	return item, tx.Commit()
}

const inboxColumns = `id,scope_key,COALESCE(task_id,0),COALESCE(conversation_id,0),main_seg,mode,state,revision,payload`

func scanInbox(rows *sql.Rows) ([]AgentInboxItem, error) {
	defer rows.Close()
	out := []AgentInboxItem{}
	for rows.Next() {
		var i AgentInboxItem
		if err := rows.Scan(&i.ID, &i.Scope, &i.TaskID, &i.ConversationID, &i.MainSeg, &i.Mode, &i.State, &i.Revision, &i.Payload); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}
func (d *DB) DrainAgentSteers(ctx context.Context, scope string, runIDs ...int64) ([]AgentInboxItem, error) {
	tx, err := d.inboxTx(ctx, scope)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT `+inboxColumns+` FROM agent_inbox WHERE scope_key=$1 AND mode='steer' AND state='pending' ORDER BY position,id LIMIT 4`, scope)
	if err != nil {
		return nil, err
	}
	out, err := scanInbox(rows)
	if err != nil {
		return nil, err
	}
	for index, i := range out {
		out[index].State = "delivered"
		if _, err = tx.ExecContext(ctx, `UPDATE agent_inbox SET state='delivered',finished_at=now() WHERE id=$1`, i.ID); err != nil {
			return nil, err
		}
	}
	if len(out) > 0 && len(runIDs) == 1 && runIDs[0] > 0 {
		var p struct {
			Message string `json:"message"`
		}
		if err = json.Unmarshal(out[len(out)-1].Payload, &p); err != nil {
			return nil, err
		}
		background, _ := json.Marshal(map[string]string{"source": "user_message", "text": p.Message})
		r, e := tx.ExecContext(ctx, `UPDATE agent_session_runs SET review_background=$3 WHERE id=$1 AND scope_key=$2 AND state='running'`, runIDs[0], scope, string(background))
		if e != nil {
			return nil, e
		}
		n, _ := r.RowsAffected()
		if n != 1 {
			return nil, ErrSessionRunConflict
		}
	}
	return out, tx.Commit()
}
func (d *DB) PendingAgentMessages() ([]AgentInboxItem, error) {
	rows, err := d.Query(`SELECT ` + inboxColumns + ` FROM (SELECT DISTINCT ON (scope_key) * FROM agent_inbox WHERE state='pending' ORDER BY scope_key,position,id) heads ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return scanInbox(rows)
}
func (d *DB) AgentInbox(scope string) ([]AgentInboxItem, error) {
	rows, err := d.Query(`SELECT `+inboxColumns+` FROM agent_inbox WHERE scope_key=$1 AND state IN ('pending','running') ORDER BY CASE WHEN state='running' THEN 0 ELSE 1 END,position,id LIMIT 33`, scope)
	if err != nil {
		return nil, err
	}
	return scanInbox(rows)
}
func (d *DB) ClaimAgentMessage(scope string, id int64) (bool, error) {
	tx, err := d.inboxTx(context.Background(), scope)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	// Revalidate head: a reorder or cancellation may have changed it since polling.
	r, err := tx.Exec(`UPDATE agent_inbox SET state='running' WHERE scope_key=$1 AND id=$2 AND state='pending' AND id=(SELECT id FROM agent_inbox WHERE scope_key=$1 AND state='pending' ORDER BY position,id LIMIT 1)`, scope, id)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, tx.Commit()
}
func (d *DB) FinishAgentMessage(id int64, state string) error {
	switch state {
	case "delivered", "cancelled", "failed", "interrupted":
	default:
		return fmt.Errorf("invalid inbox finish state")
	}
	_, err := d.Exec(`UPDATE agent_inbox SET state=$2,finished_at=now() WHERE id=$1 AND state='running'`, id, state)
	return err
}
func (d *DB) CancelAgentMessage(scope string, id int64) (bool, error) {
	tx, err := d.inboxTx(context.Background(), scope)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	r, err := tx.Exec(`UPDATE agent_inbox SET state='cancelled',finished_at=now() WHERE scope_key=$1 AND id=$2 AND state='pending'`, scope, id)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, tx.Commit()
}
func (d *DB) EditAgentMessage(scope string, id int64, revision int, payload []byte) error {
	tx, err := d.inboxTx(context.Background(), scope)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := tx.Exec(`UPDATE agent_inbox SET payload=$4,revision=revision+1 WHERE scope_key=$1 AND id=$2 AND revision=$3 AND mode='queue' AND state='pending'`, scope, id, revision, string(payload))
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return ErrInboxConflict
	}
	return tx.Commit()
}

// Reorder only pending Queue messages; Steer keeps its independent safe-boundary
// delivery. previous is an optimistic snapshot to reject stale drag operations.
func (d *DB) ReorderAgentMessages(scope string, ids, previous []int64) error {
	tx, err := d.inboxTx(context.Background(), scope)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id,position FROM agent_inbox WHERE scope_key=$1 AND mode='queue' AND state='pending' ORDER BY position,id`, scope)
	if err != nil {
		return err
	}
	current := []int64{}
	positions := []int64{}
	for rows.Next() {
		var id, pos int64
		if err = rows.Scan(&id, &pos); err != nil {
			rows.Close()
			return err
		}
		current = append(current, id)
		positions = append(positions, pos)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(ids) != len(current) || !reflect.DeepEqual(current, previous) {
		return ErrInboxConflict
	}
	seen := map[int64]bool{}
	for _, id := range current {
		seen[id] = true
	}
	for i, id := range ids {
		if !seen[id] {
			return ErrInboxConflict
		}
		delete(seen, id)
		if _, err = tx.Exec(`UPDATE agent_inbox SET position=$2 WHERE id=$1`, id, positions[i]); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (d *DB) CancelAgentSteers(scope string) error {
	_, err := d.Exec(`UPDATE agent_inbox SET state='cancelled',finished_at=now() WHERE scope_key=$1 AND mode='steer' AND state='pending'`, scope)
	return err
}
func (d *DB) CancelTaskSteers(taskID int64) error {
	_, err := d.Exec(`UPDATE agent_inbox SET state='cancelled',finished_at=now() WHERE task_id=$1 AND scope_key LIKE 'main:%' AND mode='steer' AND state='pending'`, taskID)
	return err
}
func (d *DB) HasTaskInbox(taskID int64) (bool, error) {
	var yes bool
	err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM agent_inbox WHERE task_id=$1 AND scope_key LIKE 'main:%' AND state IN ('pending','running'))`, taskID).Scan(&yes)
	return yes, err
}
func (d *DB) RecoverAgentInbox() error {
	_, err := d.Exec(`UPDATE agent_inbox SET state='interrupted',finished_at=now() WHERE state='running'`)
	return err
}
