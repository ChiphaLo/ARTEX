package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
)

func TestModelTransportWaitTimeouts(t *testing.T) {
	for _, stage := range []string{"header", "first_byte", "idle"} {
		t.Run(stage, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				if stage != "header" {
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
				}
				if stage == "idle" {
					io.WriteString(w, "first")
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
			}))
			defer srv.Close()
			var phases []string
			ctx := llm.WithCallProgress(t.Context(), func(p llm.CallProgress) { phases = append(phases, p.Phase) })
			req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL, strings.NewReader("fixture"))
			resp, err := observedModelRoundTrip(req, http.DefaultTransport, 150*time.Millisecond, 150*time.Millisecond, 150*time.Millisecond)
			if resp != nil {
				_, err = io.ReadAll(resp.Body)
				resp.Body.Close()
			}
			var timeout *modelWaitTimeout
			if !errors.As(err, &timeout) || !timeout.Timeout() {
				t.Fatalf("expected timeout, got %v", err)
			}
			if ctx.Err() != nil {
				t.Fatal("attempt timeout cancelled session")
			}
			if len(phases) == 0 {
				t.Fatal("missing status")
			}
		})
	}
}
func TestModelTransportActiveStreamHasNoTotalTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		for i := 0; i < 8; i++ {
			io.WriteString(w, "data\n")
			w.(http.Flusher).Flush()
			time.Sleep(40 * time.Millisecond)
		}
	}))
	defer srv.Close()
	var mu sync.Mutex
	var events []llm.CallProgress
	ctx := llm.WithCallScope(llm.WithCallProgress(t.Context(), func(p llm.CallProgress) { mu.Lock(); events = append(events, p); mu.Unlock() }), "compacting", 2, 3, 8)
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL, strings.NewReader("fixture"))
	resp, err := observedModelRoundTrip(req, http.DefaultTransport, 150*time.Millisecond, 150*time.Millisecond, 150*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || len(raw) != 40 {
		t.Fatal(string(raw), err)
	}
	mu.Lock()
	defer mu.Unlock()
	last := events[len(events)-1]
	if last.Stage != "compacting" || last.Chunk != 2 || last.GroupsDone != 3 || last.TotalGroups != 8 || last.BytesReceived != 40 {
		t.Fatal(last)
	}
	if !errors.Is(context.Cause(resp.Body.(*modelResponseBody).ctx), context.Canceled) {
		t.Fatal("close didn't release request context")
	}
}
func TestModelTransportUserCancellationNotReportedAsTimeout(t *testing.T) {
	entered := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(entered)
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(t.Context())
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL, nil)
	go func() { <-entered; cancel() }()
	_, err := observedModelRoundTrip(req, http.DefaultTransport, time.Second, time.Second, time.Second)
	var timeout *modelWaitTimeout
	if !errors.Is(err, context.Canceled) || errors.As(err, &timeout) {
		t.Fatal(err)
	}
}

func TestModelTransportTimeoutUsesSessionRetryBudget(t *testing.T) {
	for _, recover := range []bool{false, true} {
		t.Run(fmt.Sprint(recover), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				n := calls.Add(1)
				if recover && n >= 3 {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
					return
				}
				<-r.Context().Done()
			}))
			defer srv.Close()
			client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				return observedModelRoundTrip(r, http.DefaultTransport, 30*time.Millisecond, 30*time.Millisecond, 30*time.Millisecond)
			})}
			provider, err := llm.NewProvider(llm.Config{Format: llm.FormatOpenAI, BaseURL: srv.URL, APIKey: "fixture", Model: "fixture", HTTPClient: client})
			if err != nil {
				t.Fatal(err)
			}
			ctx := harness.WithModelRetry(t.Context(), harness.ModelRetryPolicy{Retries: 5, Delay: time.Millisecond, Retryable: retryableSessionError})
			var rows []db.Activity
			text, _, runErr := captureRun(ctx, agentcore.Options{Provider: provider, WorkingDir: t.TempDir()}, "fixture", func(a db.Activity) { rows = append(rows, a) })
			want := int32(6)
			if recover {
				want = 3
				if runErr != nil || text != "done" {
					t.Fatal(text, runErr)
				}
			} else if runErr == nil {
				t.Fatal("exhausted timeout was accepted")
			}
			if calls.Load() != want {
				t.Fatal("request attempts", calls.Load(), want)
			}
			retries, statuses := 0, 0
			for _, a := range rows {
				if a.Kind == "retry" {
					retries++
				}
				if a.Kind == "model_status" {
					statuses++
				}
			}
			if retries != int(want)-1 || statuses == 0 {
				t.Fatal("lost retry/progress events", retries, statuses)
			}
		})
	}
}
