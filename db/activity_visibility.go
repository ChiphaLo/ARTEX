package db

import (
	"database/sql"
	"strings"
)

// modelActivitySQL is the read boundary for agent tools and generated prompts,
// not the human activity feed. Keep result rows for billing/UI, but never turn
// host-generated diagnostics into a worker conclusion. The legacy prefixes
// cover records written before ui_only existed, without rewriting live history
// or examining potentially very large detail blobs.
func modelActivitySQL(alias string) string {
	prefix := ""
	if alias != "" {
		prefix = alias + "." // internal, fixed SQL aliases only
	}
	return strings.ReplaceAll(`(
 @kind NOT IN ('thinking','usage','context_usage','model_status','retry')
 AND COALESCE(@metadata->>'ui_only','false') <> 'true'
 AND NOT (@kind='text' AND @is_error)
 AND NOT (@kind='result' AND (
   COALESCE(@summary,'') LIKE '（无文字总结，终态 %'
   OR COALESCE(@summary,'') LIKE '（运行被中断：%'
   OR COALESCE(@summary,'') LIKE '（达到运行预算上限(%'
   OR (@is_error AND COALESCE(@summary,'') LIKE '执行出错: %')
 ))
)`, "@", prefix)
}

// LatestWorkerOutput reads only model-visible prose, independently of UI paging
// limits. terminalOnly closes the inherited-source reopen race in the same SQL
// statement as the activity read. Tool errors remain available via trace tools.
func (s *ExplorationStore) LatestWorkerOutput(nodeID int64, terminalOnly bool) (*Activity, error) {
	return s.latestWorkerOutput(nodeID, terminalOnly, true)
}

// LatestWorkerOutputSummary avoids loading a large body into overview prompts.
func (s *ExplorationStore) LatestWorkerOutputSummary(nodeID int64, terminalOnly bool) (*Activity, error) {
	return s.latestWorkerOutput(nodeID, terminalOnly, false)
}

func (s *ExplorationStore) latestWorkerOutput(nodeID int64, terminalOnly, includeDetail bool) (*Activity, error) {
	terminal := ""
	if terminalOnly {
		terminal = ` AND EXISTS (SELECT 1 FROM exploration_nodes n
 WHERE n.id=a.node_id AND n.exploration_id=a.exploration_id
 AND n.kind='intent' AND n.state IN ('done','blocked','exhausted','stopped'))`
	}
	detail := "''"
	if includeDetail {
		detail = "COALESCE(a.detail,'')"
	}
	var a Activity
	err := s.db.QueryRow(`SELECT a.id,a.node_id,COALESCE(a.worker,''),a.kind,a.is_error,
 COALESCE(a.summary,''),`+detail+` FROM activity a
 WHERE a.exploration_id=$1 AND a.node_id=$2 AND a.kind IN ('result','text')
 AND `+modelActivitySQL("a")+terminal+`
 ORDER BY (a.kind='result') DESC,a.id DESC LIMIT 1`, s.expID, nodeID).
		Scan(&a.ID, &a.NodeID, &a.Worker, &a.Kind, &a.IsError, &a.Summary, &a.Detail)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &a, err
}

func (s *ExplorationStore) LatestWorkerOutputWithSources(nodeID int64) (*Activity, error) {
	node, err := s.GetNode(nodeID)
	if err != nil {
		return nil, err
	}
	if node != nil {
		return s.LatestWorkerOutput(nodeID, false)
	}
	sources, err := s.DirectSourceStores()
	if err != nil {
		return nil, err
	}
	for _, source := range sources {
		a, err := source.Store.LatestWorkerOutput(nodeID, true)
		if err != nil {
			return nil, err
		}
		if a != nil {
			a.Inherited, a.SourceTaskID = true, source.Task.TaskID
			return a, nil
		}
	}
	return nil, nil
}
