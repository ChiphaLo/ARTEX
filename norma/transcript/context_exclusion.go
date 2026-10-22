package transcript

import (
	"fmt"
	"strings"

	"github.com/Autumn-27/norma/llm"
)

// Exclusions are append-only audit entries. Original messages and usage remain
// intact; stable per-block identities prevent removing identical unrelated text.
func identifyBlocks(r Record) {
	if r.Message == nil {
		return
	}
	for i := range r.Message.Content {
		id := fmt.Sprintf("%s:%d", r.UUID, i)
		if i < len(r.BlockIDs) {
			id = r.BlockIDs[i]
		}
		r.Message.Content[i].ContextID = id
	}
}

func excludedBlocks(recs []Record) map[string]bool {
	excluded := map[string]bool{}
	last := -1
	for i, r := range recs {
		identifyBlocks(r)
		if len(r.ExcludedBlockIDs) > 0 {
			last = i
		}
		for _, id := range r.ExcludedBlockIDs {
			excluded[id] = true
		}
	}
	// Summaries made before the deletion may contain deleted material. Drop
	// those boundaries AND summaries, so the next compaction uses clean sources.
	for i := 0; i < last; i++ {
		if recs[i].Message != nil && llm.IsBoundaryMarker(*recs[i].Message) {
			for _, b := range recs[i].Message.Content {
				excluded[b.ContextID] = true
			}
			for j := i + 1; j < last; j++ {
				if recs[j].Message != nil {
					for _, b := range recs[j].Message.Content {
						excluded[b.ContextID] = true
					}
					break
				}
			}
		}
	}
	return excluded
}

func filterBlocks(messages []llm.Message, excluded map[string]bool) []llm.Message {
	if len(excluded) == 0 {
		return messages
	}
	toolIDs := map[string]bool{}
	for _, m := range messages {
		for _, b := range m.Content {
			if excluded[b.ContextID] {
				if b.Type == llm.BlockToolUse {
					toolIDs[b.ID] = true
				}
				if b.Type == llm.BlockToolResult {
					toolIDs[b.ToolUseID] = true
				}
			}
		}
	}
	out := make([]llm.Message, 0, len(messages))
	// Keep the latest copy of retained blocks, not the pre-compaction copy:
	// MessagesForAPI slices at the last valid boundary.
	last := map[string]int{}
	for i, m := range messages {
		for _, b := range m.Content {
			if b.ContextID != "" {
				last[b.ContextID] = i
			}
		}
	}
	seen := map[string]bool{}
	for i, m := range messages {
		blocks := []llm.ContentBlock{}
		for _, b := range m.Content {
			if excluded[b.ContextID] || (b.Type == llm.BlockToolUse && toolIDs[b.ID]) || (b.Type == llm.BlockToolResult && toolIDs[b.ToolUseID]) {
				continue
			}
			if b.ContextID != "" && seen[b.ContextID] {
				continue
			}
			if b.ContextID != "" && last[b.ContextID] != i {
				continue
			}
			if b.ContextID != "" {
				seen[b.ContextID] = true
			}
			blocks = append(blocks, b)
		}
		if len(blocks) > 0 {
			out = append(out, llm.Message{Role: m.Role, Content: blocks})
		}
	}
	return llm.PairToolBlocks(out)
}

func (s *Store) FilterContext(session string, messages []llm.Message) ([]llm.Message, error) {
	recs, err := s.Load(s.MainPath(session))
	if err != nil {
		return nil, err
	}
	excluded := excludedBlocks(recs)
	removedDuringCompaction := false
	for _, m := range messages {
		for _, b := range m.Content {
			removedDuringCompaction = removedDuringCompaction || excluded[b.ContextID]
		}
	}
	if removedDuringCompaction {
		// A deletion can arrive while the compactor is awaiting a model response.
		// Reject its not-yet-recorded boundary/summary if it used stale sources.
		clean := make([]llm.Message, 0, len(messages))
		for i := 0; i < len(messages); i++ {
			m := messages[i]
			if llm.IsBoundaryMarker(m) && m.Content[0].ContextID == "" {
				i++ // generated summary immediately follows its boundary
				continue
			}
			clean = append(clean, m)
		}
		messages = clean
	}
	return filterBlocks(messages, excluded), nil
}

// ExcludeSelection resolves one captured step to a unique transcript block.
// A not-yet-persisted/ambiguous step is rejected instead of deleting by substring.
func (s *Store) ExcludeSelection(session, kind, text, toolID string, userProjection ...func(string) string) error {
	recs, err := s.Load(s.MainPath(session))
	if err != nil {
		return err
	}
	excluded := excludedBlocks(recs)
	ids := map[string]bool{}
	matchedTools := map[string]bool{}
	for _, r := range recs {
		if r.Message == nil {
			continue
		}
		for _, b := range r.Message.Content {
			if excluded[b.ContextID] {
				continue
			}
			matched := false
			switch kind {
			case "tool_use", "tool_result":
				matched = toolID != "" && ((b.Type == llm.BlockToolUse && b.ID == toolID) || (b.Type == llm.BlockToolResult && b.ToolUseID == toolID))
			case "thinking":
				matched = b.Type == llm.BlockThinking && strings.TrimSpace(b.Thinking) == strings.TrimSpace(text)
			case "user":
				candidate := b.Text
				if len(userProjection) > 0 && userProjection[0] != nil {
					candidate = userProjection[0](candidate)
				}
				matched = r.Message.Role == llm.RoleUser && b.Type == llm.BlockText && strings.TrimSpace(candidate) == strings.TrimSpace(text)
			case "text", "result":
				matched = r.Message.Role == llm.RoleAssistant && b.Type == llm.BlockText && strings.TrimSpace(b.Text) == strings.TrimSpace(text)
			}
			if matched {
				ids[b.ContextID] = true
				if toolID != "" {
					matchedTools[toolID] = true
				}
			}
		}
	}
	if len(ids) == 0 {
		return fmt.Errorf("内容尚未写入原始上下文、已经移除或不支持移除；请等待当前内容块结束后刷新重试")
	}
	if toolID == "" && len(ids) > 1 {
		return fmt.Errorf("存在多条相同原文，无法唯一定位；为避免误删，本次未修改")
	}
	if kind == "user" {
		for _, r := range recs {
			if r.Message == nil || r.Message.Role != llm.RoleUser {
				continue
			}
			selected := false
			for _, b := range r.Message.Content {
				if ids[b.ContextID] {
					selected = true
				}
			}
			if selected {
				for _, b := range r.Message.Content {
					if b.Type == llm.BlockImage {
						ids[b.ContextID] = true
					}
				}
			}
		}
	}
	// Both sides of a tool exchange are selected together.
	for _, r := range recs {
		if r.Message != nil {
			for _, b := range r.Message.Content {
				if (b.Type == llm.BlockToolUse && matchedTools[b.ID]) || (b.Type == llm.BlockToolResult && matchedTools[b.ToolUseID]) {
					ids[b.ContextID] = true
				}
			}
		}
	}
	list := make([]string, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}
	return s.Append(s.MainPath(session), Record{UUID: newID(), SessionID: session, Type: "context_exclusion", ExcludedBlockIDs: list})
}
