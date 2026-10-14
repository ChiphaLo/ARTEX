package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

var ErrSessionRunConflict = errors.New("会话状态已变化，或仍在运行，请刷新后重试")

type AgentSessionRun struct {
	ID       int64           `json:"run_id"`
	Scope    string          `json:"-"`
	State    string          `json:"state"`
	ReadOnly bool            `json:"read_only"`
	Review   json.RawMessage `json:"-"`
}

func (d *DB) LatestAgentRun(scope string) (*AgentSessionRun, error) {
	var r AgentSessionRun
	err := d.QueryRow(`SELECT id,scope_key,state,read_only,review_background FROM agent_session_runs WHERE scope_key=$1 ORDER BY id DESC LIMIT 1`, scope).Scan(&r.ID, &r.Scope, &r.State, &r.ReadOnly, &r.Review)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &r, err
}
func (d *DB) StartAgentRun(scope string, taskID, convID int64, readOnly bool, review []byte, resumeOf int64) (int64, error) {
	tx, err := d.inboxTx(context.Background(), scope)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var oldID int64
	var state string
	var previousReadOnly bool
	var previousReview []byte
	err = tx.QueryRow(`SELECT id,state,read_only,review_background FROM agent_session_runs WHERE scope_key=$1 ORDER BY id DESC LIMIT 1`, scope).Scan(&oldID, &state, &previousReadOnly, &previousReview)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if state == "running" || (resumeOf > 0 && (oldID != resumeOf || !ResumableAgentState(state))) {
		return 0, ErrSessionRunConflict
	}
	if resumeOf > 0 {
		readOnly = previousReadOnly
		review = previousReview
	}
	var id int64
	err = tx.QueryRow(`INSERT INTO agent_session_runs(scope_key,task_id,conversation_id,read_only,review_background,resume_of) VALUES ($1,NULLIF($2,0),NULLIF($3,0),$4,$5,NULLIF($6,0)) RETURNING id`, scope, taskID, convID, readOnly, string(review), resumeOf).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}
func ResumableAgentState(state string) bool {
	return state == "interrupted" || state == "failed" || state == "budget"
}
func (d *DB) FinishAgentRun(id int64, state string) error {
	switch state {
	case "completed", "interrupted", "failed", "budget":
	default:
		return ErrSessionRunConflict
	}
	_, err := d.Exec(`UPDATE agent_session_runs SET state=$2,finished_at=now() WHERE id=$1 AND state='running'`, id, state)
	return err
}
func (d *DB) RecoverAgentRuns() error {
	_, err := d.Exec(`UPDATE agent_session_runs SET state='interrupted',finished_at=now() WHERE state='running'`)
	return err
}

// Release only a claimed inbox message that failed admission before any model
// call/user activity. It is safe to try again after the competing planner ends.
func (d *DB) ReleaseAgentMessage(id int64) error {
	_, err := d.Exec(`UPDATE agent_inbox SET state='pending' WHERE id=$1 AND state='running'`, id)
	return err
}
