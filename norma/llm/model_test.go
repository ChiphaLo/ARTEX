package llm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func modelConfig(p Provider) Config {
	switch p := p.(type) {
	case *anthropicProvider:
		return p.cfg
	case *openaiProvider:
		return p.cfg
	case *openaiResponsesProvider:
		return p.cfg
	}
	panic("unexpected provider")
}

func TestForModelRetainsSharedResourcesAndWireModel(t *testing.T) {
	for _, format := range []Format{FormatAnthropic, FormatOpenAI, FormatOpenAIResponses} {
		t.Run(string(format), func(t *testing.T) {
			var models []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Model  string
					Stream bool
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				models = append(models, body.Model)
				if body.Stream {
					w.Header().Set("Content-Type", "text/event-stream")
					switch format {
					case FormatAnthropic:
						fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
					case FormatOpenAI:
						fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
					case FormatOpenAIResponses:
						fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
					}
				} else {
					w.Header().Set("Content-Type", "application/json")
					switch format {
					case FormatAnthropic:
						fmt.Fprint(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
					case FormatOpenAI:
						fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
					case FormatOpenAIResponses:
						fmt.Fprint(w, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`)
					}
				}
			}))
			defer srv.Close()
			base, err := NewProvider(Config{Format: format, BaseURL: srv.URL, APIKey: "fixture-key", Model: "default", RateLimit: &RateLimit{PerSecond: 10000}})
			if err != nil {
				t.Fatal(err)
			}
			variant, err := ForModel(base, " selected ")
			if err != nil {
				t.Fatal(err)
			}
			before, after := modelConfig(base), modelConfig(variant)
			if before.Model != "default" || after.Model != "selected" || before.limiter == nil || before.limiter != after.limiter || before.HTTPClient != after.HTTPClient {
				t.Fatal("model variant mutated default or replaced shared resources")
			}
			req := CompletionRequest{Messages: []Message{UserText("fixture")}}
			for _, p := range []Provider{variant, base} {
				if got := collect(t, p, req).Text(); got != "ok" {
					t.Fatalf("stream text=%q", got)
				}
				if msg, _, _, err := p.Complete(t.Context(), req); err != nil || msg.Text() != "ok" {
					t.Fatalf("complete=%+v err=%v", msg, err)
				}
			}
			if fmt.Sprint(models) != "[selected selected default default]" {
				t.Fatalf("wire models=%v", models)
			}
		})
	}
	if _, err := ForModel(nil, ""); err == nil {
		t.Fatal("empty model accepted")
	}
	if _, err := ForModel(nil, "selected"); err == nil {
		t.Fatal("unsupported provider accepted")
	}
}
