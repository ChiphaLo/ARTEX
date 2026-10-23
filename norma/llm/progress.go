package llm

import "context"

// CallProgress contains timing/volume only, never prompts, headers or model text.
type CallProgress struct {
	Stage         string `json:"stage"`
	Phase         string `json:"phase"`
	StartedAt     int64  `json:"started_at,omitempty"`
	LastDataAt    int64  `json:"last_data_at,omitempty"`
	BytesReceived int64  `json:"bytes_received,omitempty"`
	Chunk         int    `json:"chunk,omitempty"`
	GroupsDone    int    `json:"groups_done,omitempty"`
	TotalGroups   int    `json:"total_groups,omitempty"`
}
type progressKey struct{}
type progressScopeKey struct{}

func WithCallProgress(ctx context.Context, observe func(CallProgress)) context.Context {
	return context.WithValue(ctx, progressKey{}, observe)
}
func WithCallScope(ctx context.Context, stage string, chunk, done, total int) context.Context {
	return context.WithValue(ctx, progressScopeKey{}, CallProgress{Stage: stage, Chunk: chunk, GroupsDone: done, TotalGroups: total})
}
func ReportCallProgress(ctx context.Context, p CallProgress) {
	if scope, ok := ctx.Value(progressScopeKey{}).(CallProgress); ok {
		p.Stage = scope.Stage
		p.Chunk = scope.Chunk
		p.GroupsDone = scope.GroupsDone
		p.TotalGroups = scope.TotalGroups
	}
	if p.Stage == "" {
		p.Stage = "model"
	}
	if fn, ok := ctx.Value(progressKey{}).(func(CallProgress)); ok && fn != nil {
		fn(p)
	}
}
