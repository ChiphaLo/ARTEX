package agent

import (
	"context"
	"encoding/json"
	"iter"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/Autumn-27/norma/llm"
)

// Preserve upstream diagnostics verbatim except credentials, before errors reach
// retries, final results, logs or failover notices. Do not change wire captures.
func withProviderDiagnostics(provider llm.Provider, cfg Config) llm.Provider {
	key := cfg.APIKey
	if key == "" {
		if cfg.Format == llm.FormatAnthropic {
			key = os.Getenv("ANTHROPIC_API_KEY")
		} else {
			key = os.Getenv("OPENAI_API_KEY")
		}
	}
	secrets := []string{key}
	if u, err := url.Parse(cfg.Proxy); err == nil && u.User != nil {
		if password, ok := u.User.Password(); ok {
			secrets = append(secrets, u.User.String(), password)
		}
	}
	variants := map[string]bool{}
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		encoded, _ := json.Marshal(secret)
		for _, value := range []string{secret, string(encoded[1 : len(encoded)-1]), url.QueryEscape(secret), url.PathEscape(secret)} {
			variants[value] = true
		}
	}
	ordered := make([]string, 0, len(variants))
	for value := range variants {
		ordered = append(ordered, value)
	}
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	pairs := make([]string, 0, len(ordered)*2)
	for _, value := range ordered {
		pairs = append(pairs, value, "[REDACTED]")
	}
	return &diagnosticProvider{Provider: provider, secrets: strings.NewReplacer(pairs...)}
}

var diagnosticAuth = regexp.MustCompile(`(?i)(authorization["']?\s*[:=]\s*["']?(?:bearer|basic)\s+)[^\s"',;}]+`)
var diagnosticSecret = regexp.MustCompile(`(?i)((?:api[_-]?key|access[_-]?token|refresh[_-]?token|client[_-]?secret|password)["']?\s*[:=]\s*["']?)[^\s"',;}]+`)

type diagnosticProvider struct {
	llm.Provider
	secrets *strings.Replacer
}

type diagnosticError struct {
	cause error
	text  string
}

func (e *diagnosticError) Error() string { return e.text }
func (e *diagnosticError) Unwrap() error { return e.cause }

func (p *diagnosticProvider) safeError(err error) error {
	if err == nil {
		return nil
	}
	text := p.secrets.Replace(err.Error())
	text = diagnosticAuth.ReplaceAllString(text, "${1}[REDACTED]")
	text = diagnosticSecret.ReplaceAllString(text, "${1}[REDACTED]")
	if text == err.Error() {
		return err
	}
	return &diagnosticError{cause: err, text: text}
}

func (p *diagnosticProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		for event, err := range p.Provider.Stream(ctx, req) {
			if !yield(event, p.safeError(err)) {
				return
			}
		}
	}
}

func (p *diagnosticProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	message, stop, usage, err := p.Provider.Complete(ctx, req)
	return message, stop, usage, p.safeError(err)
}
