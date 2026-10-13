package agent

import (
	"context"

	"github.com/Autumn-27/norma/llm"
)

type liveMessagesKey struct{}

// WithLiveMessages binds a human-only host inbox to one run. Worker/system
// summaries must not be supplied through this channel. Queue is dispatched by
// the server after this run, not consumed here.
func WithLiveMessages(ctx context.Context, drain func(context.Context) []llm.Message) context.Context {
	return context.WithValue(ctx, liveMessagesKey{}, drain)
}
func liveMessages(ctx context.Context) func(context.Context) []llm.Message {
	drain, _ := ctx.Value(liveMessagesKey{}).(func(context.Context) []llm.Message)
	return drain
}
