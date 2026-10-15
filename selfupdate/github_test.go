package selfupdate

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type releaseTransport func(*http.Request) (*http.Response, error)

func (f releaseTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFetchLatestUsesForkAndDistinguishesNoRelease(t *testing.T) {
	if Repo != "ChiphaLo/ARTEX" {
		t.Fatalf("unexpected update source %q", Repo)
	}
	for _, tc := range []struct {
		name                     string
		status, repositoryStatus int
		body                     string
		wantNoRelease, wantError bool
	}{
		{"release", 200, 0, `{"tag_name":"v1.2.3"}`, false, false},
		{"empty channel", 404, 200, `{}`, true, true},
		{"missing repository", 404, 404, `{}`, false, true},
		{"rate limited", 403, 0, `{}`, false, true},
		{"server failure", 500, 0, `{}`, false, true},
		{"bad json", 200, 0, `{`, false, true},
		{"missing tag", 200, 0, `{}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := &http.Client{Transport: releaseTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				want := "https://api.github.com/repos/ChiphaLo/ARTEX/releases/latest"
				status := tc.status
				if calls == 2 {
					want = "https://api.github.com/repos/ChiphaLo/ARTEX"
					status = tc.repositoryStatus
				}
				if calls > 2 || r.URL.String() != want || r.Method != http.MethodGet {
					t.Fatalf("unexpected request %s %s", r.Method, r.URL)
				}
				if r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("User-Agent") == "" {
					t.Fatal("missing GitHub request headers")
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
			})}
			rel, err := FetchLatest(context.Background(), c)
			if (err != nil) != tc.wantError || errors.Is(err, ErrNoRelease) != tc.wantNoRelease {
				t.Fatalf("err=%v; want error=%v noRelease=%v", err, tc.wantError, tc.wantNoRelease)
			}
			if !tc.wantError && (rel == nil || rel.TagName != "v1.2.3") {
				t.Fatalf("release=%+v", rel)
			}
			wantCalls := 1
			if tc.status == 404 {
				wantCalls = 2
			}
			if calls != wantCalls {
				t.Fatalf("calls=%d want=%d", calls, wantCalls)
			}
		})
	}
}
