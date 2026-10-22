package agent

import (
	"context"
	"sync"

	"github.com/Autumn-27/norma/llm"
)

type inputContentKey struct{}
type inputContent struct {
	mu   sync.Mutex
	load func() ([]llm.ContentBlock, error)
}

// WithInputContent attaches images to the first prompt only, not settlement prompts.
func WithInputContent(ctx context.Context, load func() ([]llm.ContentBlock, error)) context.Context {
	return context.WithValue(ctx, inputContentKey{}, &inputContent{load: load})
}
func takeInputContent(ctx context.Context) ([]llm.ContentBlock, error) {
	c, _ := ctx.Value(inputContentKey{}).(*inputContent)
	if c == nil {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	load := c.load
	c.load = nil
	if load == nil {
		return nil, nil
	}
	return load()
}

// InheritInputContent preserves the per-message payload across a cached task
// cancellation context, without inheriting another worker's input.
func InheritInputContent(ctx, source context.Context) context.Context {
	c, _ := source.Value(inputContentKey{}).(*inputContent)
	return context.WithValue(ctx, inputContentKey{}, c)
}
