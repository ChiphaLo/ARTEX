package llm

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUpstreamErrorBodiesAreNotReducedToMessageOrTruncated(t *testing.T) {
	raw := `{"type":"error","error":{"message":"temporarily unavailable","code":"upstream_code","detail":"` + strings.Repeat("diagnostic ", 800) + `"},"requestId":"fixture-request-id"}`
	for _, format := range []Format{FormatOpenAI, FormatOpenAIResponses, FormatAnthropic} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream-%v", format, stream), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprintf(w, "event: error\ndata: %s\n\n", raw)
					} else {
						_, _ = io.WriteString(w, raw)
					}
				}))
				defer srv.Close()
				p, err := NewProvider(Config{Format: format, Model: "fixture", APIKey: "fixture-key", BaseURL: srv.URL})
				if err != nil {
					t.Fatal(err)
				}
				ctx := WithExternalRetry(t.Context())
				req := CompletionRequest{Messages: []Message{UserText("fixture")}}
				if stream {
					for _, e := range p.Stream(ctx, req) {
						if e != nil {
							err = e
						}
					}
				} else {
					_, _, _, err = p.Complete(ctx, req)
				}
				if err == nil || !strings.Contains(err.Error(), raw) {
					t.Fatalf("upstream details lost: %v", err)
				}
			})
		}
	}
}

func TestOpenAIStreamErrorDoesNotMistakeTextForError(t *testing.T) {
	for _, raw := range []string{`{"error":null,"choices":[]}`, `{"choices":[{"delta":{"content":"an error example"}}]}`, `{"choices":[{"delta":{"content":"{\"error\":true}"}}]}`} {
		if err := openAIStreamError("", raw); err != nil {
			t.Fatal(err)
		}
	}
	raw := `{"type":"response.failed","response":{"error":{"code":"upstream_code","message":"failure"}},"requestId":"fixture"}`
	if _, _, err := parseResponsesFrame("response.failed", raw); err == nil || !strings.Contains(err.Error(), raw) {
		t.Fatal(err)
	}
}
