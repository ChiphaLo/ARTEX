package db

import (
	"fmt"
	"strconv"
)

// MainWorkerBatch belongs to a specific main conversation segment, not the
// currently selected segment. Only this turn's directly dispatched intents count.
type MainWorkerBatch struct {
	ID            int64
	ExplorationID int64
	TaskID        int64
	MainSeg       int
}

// MainWorkerResult contains bounded conclusions, never a full worker trace.
type MainWorkerResult struct {
	IntentID int64  `json:"intent_id"`
	State    string `json:"state"`
	Summary  string `json:"summary"`
	Output   string `json:"output"`
	IsError  bool   `json:"is_error"`
}

func (s *ExplorationStore) BeginMainWorkerBatch(seg int) (int64, error) {
	if seg < 0 {
		return 0, fmt.Errorf("invalid main segment")
	}
	var id int64
	err := s.db.QueryRow(`INSERT INTO main_worker_batches(exploration_id,main_seg) VALUES ($1,$2) RETURNING id`, s.expID, seg).Scan(&id)
	return id, err
}

func (s *ExplorationStore) SealMainWorkerBatch(id int64, cancelled bool) error {
	state := "waiting"
	if cancelled {
		state = "cancelled"
	}
	_, err := s.db.Exec(`UPDATE main_worker_batches SET state=$3
WHERE id=$1 AND exploration_id=$2 AND state='collecting'`, id, s.expID, state)
	if err != nil {
		return err
	}
	// Read-only turns must not produce empty notifications or accumulate rows.
	_, err = s.db.Exec(`DELETE FROM main_worker_batches b WHERE b.id=$1 AND b.exploration_id=$2
AND b.state IN ('waiting','cancelled') AND NOT EXISTS (
 SELECT 1 FROM exploration_nodes n WHERE n.exploration_id=b.exploration_id AND n.kind='intent'
 AND n.payload->>'main_batch_id'=b.id::text)`, id, s.expID)
	return err
}

// Recovery seals dispatches interrupted before normal return. A delivery that
// was already running is NOT replayed: LLM/tool side effects cannot be made
// exactly-once across a process crash. It stays visibly interrupted in the DB.
func (d *DB) RecoverMainWorkerBatches() error {
	_, err := d.Exec(`UPDATE main_worker_batches SET state=CASE WHEN state='collecting' THEN 'waiting' ELSE 'interrupted' END
WHERE state IN ('collecting','delivering')`)
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM main_worker_batches b WHERE state='waiting' AND NOT EXISTS (
SELECT 1 FROM exploration_nodes n WHERE n.exploration_id=b.exploration_id AND n.kind='intent'
AND n.payload->>'main_batch_id'=b.id::text)`)
	return err
}

const mainBatchReady = `EXISTS (SELECT 1 FROM exploration_nodes n
 WHERE n.exploration_id=b.exploration_id AND n.kind='intent' AND n.payload ? 'main_batch_id' AND n.payload->>'main_batch_id'=b.id::text)
AND NOT EXISTS (SELECT 1 FROM exploration_nodes n
 WHERE n.exploration_id=b.exploration_id AND n.kind='intent' AND n.payload ? 'main_batch_id' AND n.payload->>'main_batch_id'=b.id::text
 AND n.state NOT IN ('done','blocked','exhausted','stopped','deleted'))`

func (d *DB) ReadyMainWorkerBatches(limit int) ([]MainWorkerBatch, error) {
	rows, err := d.Query(`WITH ready AS (
SELECT b.id,b.exploration_id,t.id AS task_id,b.main_seg FROM main_worker_batches b
JOIN tasks t ON t.exploration_id=b.exploration_id
WHERE b.state='waiting' AND NOT t.paused AND NOT t.queued AND `+mainBatchReady+`
), heads AS (SELECT DISTINCT ON (exploration_id) * FROM ready ORDER BY exploration_id,id)
SELECT id,exploration_id,task_id,main_seg FROM heads ORDER BY id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MainWorkerBatch
	for rows.Next() {
		var b MainWorkerBatch
		if err := rows.Scan(&b.ID, &b.ExplorationID, &b.TaskID, &b.MainSeg); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Claim rechecks all worker states and task pause atomically with the state CAS.
// An event burst or concurrent pump can only admit one delivery.
func (d *DB) ClaimMainWorkerBatch(id int64) (bool, error) {
	r, err := d.Exec(`UPDATE main_worker_batches b SET state='delivering' WHERE b.id=$1 AND b.state='waiting'
AND EXISTS (SELECT 1 FROM tasks t WHERE t.exploration_id=b.exploration_id AND NOT t.paused AND NOT t.queued)
AND `+mainBatchReady, id)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}

func (d *DB) FinishMainWorkerBatch(id int64, state string) error {
	switch state {
	case "delivered", "failed", "cancelled", "interrupted":
	default:
		return fmt.Errorf("invalid delivery state")
	}
	_, err := d.Exec(`UPDATE main_worker_batches SET state=$2,finished_at=now() WHERE id=$1 AND state='delivering'`, id, state)
	return err
}

// Stop suppresses future automatic turns, not the independently running workers.
func (s *ExplorationStore) CancelMainWorkerBatches() error {
	_, err := s.db.Exec(`UPDATE main_worker_batches SET state='cancelled',finished_at=now()
WHERE exploration_id=$1 AND state IN ('collecting','waiting','delivering')`, s.expID)
	return err
}

func (d *DB) MainWorkerBatchResults(b MainWorkerBatch) ([]MainWorkerResult, int, error) {
	var total int
	key := strconv.FormatInt(b.ID, 10)
	if err := d.QueryRow(`SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent'
AND payload->>'main_batch_id'=$2`, b.ExplorationID, key).Scan(&total); err != nil {
		return nil, 0, err
	}
	// Prefer the LAST result, not the first 1,000 activities. Bound in SQL before
	// loading large output; the caller also imposes a total prompt size bound.
	rows, err := d.Query(`SELECT n.id,n.state,left(COALESCE(n.payload->>'summary',''),600),
left(COALESCE(NULLIF(a.detail,''),a.summary,n.delete_reason,n.blocked_reason,''),2400),COALESCE(a.is_error,false)
FROM exploration_nodes n LEFT JOIN LATERAL (
 SELECT detail,summary,is_error FROM activity WHERE exploration_id=n.exploration_id AND node_id=n.id
 AND kind IN ('result','text') ORDER BY (kind='result') DESC,id DESC LIMIT 1
) a ON true WHERE n.exploration_id=$1 AND n.kind='intent' AND n.payload->>'main_batch_id'=$2
ORDER BY n.id LIMIT 64`, b.ExplorationID, key)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []MainWorkerResult
	for rows.Next() {
		var r MainWorkerResult
		if err := rows.Scan(&r.IntentID, &r.State, &r.Summary, &r.Output, &r.IsError); err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}
