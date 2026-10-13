package intercept

import (
	"context"
	"encoding/json"
	"testing"
)

func TestReviewLiveHumanBackgroundPreservesDirectoryAndFiltersSource(t *testing.T) {
	background := ReviewBackground{Source: BackgroundUserMessage, Text: "original"}
	ctx := WithReviewContext(context.Background(), "old", background)
	ctx = WithReviewBackgroundFunc(ctx, func() ReviewBackground { return background })
	ctx = WithReviewWorkingDirectory(ctx, "current")
	input := func() ReviewInput {
		t.Helper()
		in, err := BuildReviewInput(ctx, "Read", json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if in.WorkingDir != "current" {
			t.Fatal(in)
		}
		return in
	}
	if input().Background.Text != "original" {
		t.Fatal("initial background lost")
	}
	background = ReviewBackground{Source: BackgroundUserMessage, Text: "consumed human steer"}
	if input().Background.Text != background.Text {
		t.Fatal("live background lost")
	}
	background = ReviewBackground{Source: "worker_completion", Text: "generated worker text"}
	if input().Background != nil {
		t.Fatal("worker text became authority")
	}
}
