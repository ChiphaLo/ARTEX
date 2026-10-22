package llm

import (
	"fmt"
	"strings"
)

// ForModel creates an immutable model variant, retaining the original HTTP
// client and rate limiter. It never changes the shared provider's default.
func ForModel(provider Provider, model string) (Provider, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, fmt.Errorf("model is required")
	}
	if p, ok := provider.(interface {
		ForModel(string) (Provider, error)
	}); ok {
		return p.ForModel(model)
	}
	return nil, fmt.Errorf("provider %T does not support model variants", provider)
}

func (p *anthropicProvider) ForModel(model string) (Provider, error) {
	copy := *p
	copy.cfg.Model = model
	return &copy, nil
}

func (p *openaiProvider) ForModel(model string) (Provider, error) {
	copy := *p
	copy.cfg.Model = model
	return &copy, nil
}

func (p *openaiResponsesProvider) ForModel(model string) (Provider, error) {
	copy := *p
	copy.cfg.Model = model
	return &copy, nil
}
