package agent

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

func TestProviderDiagnosticsPreserveErrorsAndRedactCredentials(t *testing.T) {
	const key = "fixture-configured-api-key"
	const other = "fixture-upstream-secret"
	raw := "\n {\"error\":{\"code\":\"upstream_error\",\"message\":\"" + strings.Repeat("gateway diagnostic ", 400) + "\",\"api_key\":\"" + key + "\",\"authorization\":\"Bearer " + other + "\"},\"requestId\":\"fixture-request-id\"}\n"
	want := strings.ReplaceAll(strings.ReplaceAll(raw, key, "[REDACTED]"), other, "[REDACTED]")
	for _, format := range []llm.Format{llm.FormatOpenAI, llm.FormatOpenAIResponses, llm.FormatAnthropic} {
		for _, syncMode := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/sync-%v", format, syncMode), func(t *testing.T) {
				calls := 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					w.WriteHeader(503)
					_, _ = io.WriteString(w, raw)
				}))
				defer srv.Close()
				p, err := (Config{Format: format, Model: "fixture", BaseURL: srv.URL, APIKey: key}).NewProvider()
				if err != nil {
					t.Fatal(err)
				}
				ctx := llm.WithExternalRetry(t.Context())
				req := llm.CompletionRequest{Messages: []llm.Message{llm.UserText("fixture")}}
				if syncMode {
					_, _, _, err = p.Complete(ctx, req)
				} else {
					for _, e := range p.Stream(ctx, req) {
						if e != nil {
							err = e
						}
					}
				}
				if err == nil || !strings.Contains(err.Error(), "status 503") || !strings.HasSuffix(err.Error(), want) || calls != 1 {
					t.Fatalf("diagnostic lost: calls=%d err=%v", calls, err)
				}
				if strings.Contains(err.Error(), key) || strings.Contains(err.Error(), other) {
					t.Fatal("credential leaked")
				}
			})
		}
	}
}

func TestProviderDiagnosticErrorUnwrapAndUnchangedText(t *testing.T) {
	p := withProviderDiagnostics(nil, Config{APIKey: "fixture-key", Proxy: "http://fixture-user:fixture-pass@example.test"}).(*diagnosticProvider)
	plain := errors.New("dial tcp 127.0.0.1:443: connection refused")
	if p.safeError(plain) != plain || p.safeError(nil) != nil {
		t.Fatal("ordinary network error changed")
	}
	cause := errors.New("proxy http://fixture-user:fixture-pass@example.test: refused; api_key=fixture-key")
	err := p.safeError(cause)
	if !errors.Is(err, cause) || strings.Contains(err.Error(), "fixture-pass") || strings.Contains(err.Error(), "fixture-key") || !strings.Contains(err.Error(), "example.test") {
		t.Fatal(err)
	}
}
