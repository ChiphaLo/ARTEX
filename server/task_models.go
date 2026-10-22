package server

import (
	"log"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/sidequestion"
	"github.com/Autumn-27/norma/llm"
)

func profileHasModel(p *db.LLMProfile, model string) bool {
	if p == nil {
		return false
	}
	if p.Model == model {
		return true
	}
	for _, candidate := range p.Models {
		if candidate == model {
			return true
		}
	}
	return false
}

func selectedTaskModel(state taskLLMState) string {
	if state.ActiveID == nil {
		return ""
	}
	return state.Models[*state.ActiveID]
}

func (s *Server) taskLLMAuditProfile(id int64, models map[int64]string) *llmAuditProfile {
	p := s.llmAuditProfile(id)
	if p != nil && models[id] != "" {
		p.Model = models[id]
	}
	return p
}

func (s *Server) providerForProfileModel(id int64, model string) (llm.Provider, agent.Config, bool) {
	provider, cfg, ok := s.providerForProfile(id)
	model = strings.TrimSpace(model)
	if !ok || model == "" {
		return provider, cfg, ok
	}
	p, err := s.m.pg.ProfileByID(id)
	if err != nil || !profileHasModel(p, model) {
		return nil, agent.Config{}, false
	}
	cfg.Model = model
	binding := sideModel(cfg, id, p.Name)
	binding.Pinned = true
	variant, err := sidequestion.RebindModel(provider, binding)
	if err != nil {
		log.Printf("[task-llm] model variant for profile %d: %v", id, err)
		return nil, agent.Config{}, false
	}
	return variant, cfg, true
}
