package harness

import (
	"context"
	"time"
)

type modelRetryKey struct{}

// ModelRetryPolicy is opt-in: the host decides which failures are transient.
// Retrying happens inside the existing loop, never by replaying an intent.
type ModelRetryPolicy struct {
	Retries   int
	Delay     time.Duration
	Retryable func(error) bool
}

func WithModelRetry(ctx context.Context, policy ModelRetryPolicy) context.Context {
	return context.WithValue(ctx, modelRetryKey{}, policy)
}

func ModelRetryManaged(ctx context.Context) bool {
	_, ok := ctx.Value(modelRetryKey{}).(ModelRetryPolicy)
	return ok
}

func modelRetryPolicy(ctx context.Context) ModelRetryPolicy {
	p, _ := ctx.Value(modelRetryKey{}).(ModelRetryPolicy)
	return p
}
