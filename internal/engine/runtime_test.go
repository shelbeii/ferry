package engine

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/rundist"
)

type runtimeCatalogFixture string

func (body runtimeCatalogFixture) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(string(body))),
		Request:    req,
	}, nil
}

func TestRuntimeDownloadReportsGitHubMirror(t *testing.T) {
	const path = "/downloads/https://github.com/example/python.tar.gz"
	manifest := `{"version":"3.14.0","date":"2025-10-07","artifacts":[{"platform":"x86_64-unknown-linux-gnu","variant":"install_only_stripped","archive_format":"tar.gz","url":"https://github.com/example/python.tar.gz","sha256":"` + strings.Repeat("a", 64) + `"}]}`
	oldClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: runtimeCatalogFixture(manifest)}
	t.Cleanup(func() { http.DefaultClient = oldClient })
	for _, tc := range []struct {
		name, cause string
		status      int
	}{
		{"not found", "HTTP 404", http.StatusNotFound},
		{"HTML instead of archive", "与发行清单的摘要不符", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != path {
					t.Errorf("unexpected mirror request %q", r.URL.Path)
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, "<html>Download page</html>")
			}))
			defer mirror.Close()
			cache := t.TempDir()
			e := &Engine{Rep: &recRep{}}
			e.Settings.Runtime.GitHubMirror = mirror.URL + "/downloads/"
			_, _, _, err := e.runtimePackage(cache, rundist.Selection{Kind: "python", Version: "3.14.0", Arch: "x64"}, "")
			if err == nil || !strings.Contains(err.Error(), "GitHub 加速") || !strings.Contains(err.Error(), tc.cause) {
				t.Fatalf("runtimePackage error = %v", err)
			}
			if files, readErr := os.ReadDir(cache); readErr != nil || len(files) != 0 {
				t.Fatalf("failed download left cache contents %v, %v", files, readErr)
			}
		})
	}
}
