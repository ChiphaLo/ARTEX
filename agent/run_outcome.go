package agent

import (
	"context"

	"github.com/Autumn-27/norma/harness"
)

type outcomeKey struct{}

// WithRunOutcome observes terminal reasons without exposing a model-callable tool.
func WithRunOutcome(ctx context.Context, fn func(harness.TerminalReason)) context.Context {
	return context.WithValue(ctx, outcomeKey{}, fn)
}
func reportRunOutcome(ctx context.Context, reason harness.TerminalReason) {
	if fn, ok := ctx.Value(outcomeKey{}).(func(harness.TerminalReason)); ok && fn != nil {
		fn(reason)
	}
}

type continuationKey struct{}

func WithContinuation(ctx context.Context) context.Context {
	return context.WithValue(ctx, continuationKey{}, true)
}
func continuation(ctx context.Context) bool {
	yes, _ := ctx.Value(continuationKey{}).(bool)
	return yes
}
