package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/norma/transcript"
)

// Only local activities in this task/conversation are mutable. No client-supplied
// transcript path, text or session identity is accepted.
func (s *Server) excludeContext(w http.ResponseWriter, r *http.Request) {
	seq, err := strconv.ParseInt(r.PathValue("seq"), 10, 64)
	if err != nil || seq <= 0 {
		writeErr(w, 400, "invalid activity")
		return
	}
	var kind, detail, toolID, worker, streamID string
	var session string
	var baseDir string
	var mark func() error
	pg := s.m.pg
	if r.PathValue("id") != "" {
		_, conv, ok := s.convByID(w, r)
		if !ok {
			return
		}
		err = pg.QueryRow(`SELECT COALESCE(kind,''),CASE WHEN kind='user' THEN COALESCE(NULLIF(detail,''),summary,'') ELSE COALESCE(detail,'') END,COALESCE(tool_use_id,''),COALESCE(metadata->>'stream_id','') FROM conversation_activities WHERE id=$1 AND conversation_id=$2`, seq, conv.ID).Scan(&kind, &detail, &toolID, &streamID)
		session = s.convBusyKey(conv.ID)
		worker = conv.AgentKey
		baseDir = filepath.Join(s.m.dir, "sessions", session)
		mark = func() error {
			_, e := pg.Exec(`UPDATE conversation_activities SET metadata=metadata || '{"context_excluded":true}'::jsonb WHERE conversation_id=$1 AND (id=$2 OR ($3<>'' AND tool_use_id=$3) OR ($4<>'' AND metadata->>'stream_id'=$4))`, conv.ID, seq, toolID, streamID)
			return e
		}
	} else {
		t := s.m.ResolveTask(r.URL.Query().Get("task"))
		if t == nil {
			writeErr(w, 404, "task not found")
			return
		}
		var nodeID int64
		baseDir = filepath.Join(s.m.dir, "tasks", t.ID)
		var seg int
		err = pg.QueryRow(`SELECT COALESCE(kind,''),CASE WHEN kind='user' THEN COALESCE(NULLIF(detail,''),summary,'') ELSE COALESCE(detail,'') END,COALESCE(tool_use_id,''),COALESCE(worker,''),COALESCE(node_id,0),COALESCE(main_seg,0),COALESCE(metadata->>'stream_id','') FROM activity WHERE id=$1 AND exploration_id=$2`, seq, t.ExpID).Scan(&kind, &detail, &toolID, &worker, &nodeID, &seg, &streamID)
		if err != nil {
			writeErr(w, 404, "activity not found in this session")
			return
		}
		switch {
		case worker == "mainagent":
			session = fmt.Sprintf("exp%d-main", t.ExpID)
			if seg > 0 {
				session += fmt.Sprintf("-s%d", seg)
			}
		case worker == "planner":
			session = fmt.Sprintf("exp%d-planner", t.ExpID)
		case nodeID > 0:
			session = agent.WorkerSessionID(t.ExpID, nodeID)
		default:
			writeErr(w, 400, "这条系统活动不是可移除的模型上下文")
			return
		}
		mark = func() error {
			_, e := pg.Exec(`UPDATE activity SET metadata=COALESCE(metadata,'{}'::jsonb) || '{"context_excluded":true}'::jsonb WHERE exploration_id=$1 AND (id=$2 OR (worker=$5 AND COALESCE(node_id,0)=$6 AND COALESCE(main_seg,0)=$7 AND (($3<>'' AND tool_use_id=$3) OR ($4<>'' AND metadata->>'stream_id'=$4))))`, t.ExpID, seq, toolID, streamID, worker, nodeID, seg)
			return e
		}
	}
	if err != nil {
		writeErr(w, 404, "activity not found in this session")
		return
	}
	if detail == "" && streamID != "" {
		writeErr(w, 409, "思考块仍在流式输出，请等待该块结束后移除")
		return
	}
	store := transcript.NewStore(filepath.Join(s.m.dir, "transcripts"))
	var projectUser func(string) string
	if kind == "user" {
		var attached struct {
			Text        string           `json:"text"`
			Attachments []chatAttachment `json:"attachments"`
		}
		if json.Unmarshal([]byte(detail), &attached) == nil && len(attached.Attachments) > 0 {
			detail = composeAgentMessage(attached.Text, attached.Attachments, baseDir)
		}
		projectUser = func(candidate string) string {
			candidate = strings.TrimPrefix(candidate, liveSteerPrefix)
			// Match the complete host-generated prefix + human input + delimiter,
			// never a substring of arbitrary tool/model output.
			if worker == "planner" && strings.HasPrefix(candidate, agent.PlannerHumanPrefix+detail+agent.PlannerSituationMarker) {
				return detail
			}
			return candidate
		}
	}
	if err = store.ExcludeSelection(session, kind, detail, toolID, projectUser); err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	if err = mark(); err != nil {
		writeErr(w, 500, "上下文已移除，但展示标记写入失败，请刷新会话")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "applies": "next_safe_boundary", "audit_retained": true})
}
