package mcphttp

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
)

// Options is per connection: a private CA never changes global HTTPS trust.
type Options struct {
	Insecure bool
	CACert   string // PEM file path; may use the same explicit references as headers
}

var secretRef = regexp.MustCompile(`\$\{(env|file):([^{}]+)\}`)

// ResolveReferences resolves explicit ${env:NAME} / ${file:/path} references.
// Plain values and unrelated ${...} syntax keep their historical meaning.
// Never include either the supplied value or file contents in an error.
func ResolveReferences(value string) (string, error) {
	var first error
	out := secretRef.ReplaceAllStringFunc(value, func(ref string) string {
		match := secretRef.FindStringSubmatch(ref)
		var v string
		if match[1] == "env" {
			v = os.Getenv(match[2])
		} else {
			b, err := os.ReadFile(match[2])
			if err != nil {
				first = fmt.Errorf("MCP secret file is unavailable")
				return ""
			}
			v = strings.TrimRight(string(b), "\r\n")
		}
		if v == "" {
			first = fmt.Errorf("MCP secret reference is empty or unset")
		}
		return v
	})
	return out, first
}

func newHTTPClient(headers map[string]string, opts Options) (*http.Client, map[string]string, error) {
	resolved := make(map[string]string, len(headers))
	for k, v := range headers {
		s, err := ResolveReferences(v)
		if err != nil {
			return nil, nil, err
		}
		if strings.ContainsAny(s, "\r\n") {
			return nil, nil, fmt.Errorf("invalid MCP HTTP header value")
		}
		resolved[k] = s
	}
	clean, err := normalizeHeaders(resolved)
	if err != nil {
		return nil, nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	// Preserve same-origin redirects (e.g. /mcp -> /mcp/) without forwarding
	// credentials to another authority or downgrading HTTPS.
	hc := &http.Client{Transport: tr, CheckRedirect: func(next *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("MCP stopped after ten redirects")
		}
		if len(via) > 0 && (next.URL.Host != via[0].URL.Host || next.URL.Scheme != via[0].URL.Scheme) {
			return http.ErrUseLastResponse
		}
		return nil
	}}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: opts.Insecure}
	if opts.CACert != "" {
		path, err := ResolveReferences(opts.CACert)
		if err != nil {
			return nil, nil, fmt.Errorf("MCP CA reference: %w", err)
		}
		pem, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, fmt.Errorf("MCP CA certificate file is unavailable")
		}
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, nil, fmt.Errorf("MCP CA certificate contains no valid PEM certificate")
		}
		cfg.RootCAs = roots
	}
	tr.TLSClientConfig = cfg
	return hc, clean, nil
}

func (c *Client) safeError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	for _, v := range c.headers {
		if v == "" {
			continue
		}
		msg = strings.ReplaceAll(msg, v, "[REDACTED]")
		if strings.HasPrefix(v, "Bearer ") {
			msg = strings.ReplaceAll(msg, strings.TrimPrefix(v, "Bearer "), "[REDACTED]")
		}
	}
	if msg == err.Error() {
		return err
	}
	return fmt.Errorf("%s", msg)
}
