package llm

import "context"

type externalRetryKey struct{}

// An enclosing session loop owns the retry budget/cadence. SDK retries would
// multiply that budget and hide progress from the host.
func WithExternalRetry(ctx context.Context) context.Context {
	return context.WithValue(ctx, externalRetryKey{}, true)
}
func externalRetry(ctx context.Context) bool { v, _ := ctx.Value(externalRetryKey{}).(bool); return v }
