package github

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFileContentsDecodesBase64(t *testing.T) {
	const fileText = "module example\n\ngo.opentelemetry.io/otel/sdk v1.41.0\n"
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing bearer: %q", r.Header.Get("Authorization"))
		}
		// GitHub base64-encodes file content with embedded newlines.
		enc := base64.StdEncoding.EncodeToString([]byte(fileText))
		enc = enc[:20] + "\n" + enc[20:]
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"type":"file","encoding":"base64","content":%q}`, enc)
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL)
	got, err := c.FileContents(context.Background(), "tok", "kubernetes/autoscaler", "cluster-autoscaler/go.mod", "master", 0)
	if err != nil {
		t.Fatalf("FileContents: %v", err)
	}
	if got != fileText {
		t.Fatalf("contents = %q, want %q", got, fileText)
	}
	if gotPath != "/repos/kubernetes/autoscaler/contents/cluster-autoscaler/go.mod" {
		t.Errorf("path = %q", gotPath)
	}
	if gotQuery != "ref=master" {
		t.Errorf("query = %q, want ref=master", gotQuery)
	}
}

func TestFileContentsDirectoryIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A directory responds with a JSON array.
		_, _ = w.Write([]byte(`[{"name":"a.go","type":"file"}]`))
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL)
	got, err := c.FileContents(context.Background(), "tok", "o/r", "dir", "", 0)
	if err != nil {
		t.Fatalf("FileContents: %v", err)
	}
	if !strings.Contains(got, "directory") {
		t.Errorf("expected a directory note, got %q", got)
	}
}

// TestFileContentsPagesLargeFileWithOffset proves a file larger than one window
// is reachable in full via offset paging: page 1 reports the file size and the
// next offset; page 2 returns the remainder. Plain ASCII so byte offsets equal
// rune offsets.
func TestFileContentsPagesLargeFileWithOffset(t *testing.T) {
	fileText := strings.Repeat("A", 32<<10) + strings.Repeat("B", 4096) // 36864 bytes
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := base64.StdEncoding.EncodeToString([]byte(fileText))
		fmt.Fprintf(w, `{"type":"file","encoding":"base64","content":%q}`, enc)
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL)

	page1, err := c.FileContents(context.Background(), "tok", "o/r", "big.txt", "", 0)
	if err != nil {
		t.Fatalf("FileContents page 1: %v", err)
	}
	if !strings.Contains(page1, "request offset 32768 to continue") {
		t.Fatalf("page 1 missing continuation marker")
	}
	if strings.Contains(page1, "B") {
		t.Errorf("page 1 should not include the second half")
	}

	page2, err := c.FileContents(context.Background(), "tok", "o/r", "big.txt", "", 32<<10)
	if err != nil {
		t.Fatalf("FileContents page 2: %v", err)
	}
	if !strings.Contains(page2, "showing 32768-36864") {
		t.Errorf("page 2 missing window marker")
	}
	if !strings.Contains(page2, "BBBB") || strings.Contains(page2, "AAAA") {
		t.Errorf("page 2 should contain only the second half")
	}
}

func TestPullRequestStatusSummarizesMerge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/kubernetes/autoscaler/pulls/9484" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"number":9484,"state":"closed","merged":true,"merged_at":"2026-05-01T00:00:00Z","title":"bump grpc","base":{"ref":"master"},"head":{"ref":"fix"},"html_url":"https://github.com/kubernetes/autoscaler/pull/9484"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL)
	got, err := c.PullRequestStatus(context.Background(), "tok", "kubernetes/autoscaler", 9484)
	if err != nil {
		t.Fatalf("PullRequestStatus: %v", err)
	}
	if !strings.Contains(got, "merged") || !strings.Contains(got, "#9484") || !strings.Contains(got, "base=master") {
		t.Fatalf("summary missing fields: %q", got)
	}
}

func TestSearchFormatsMatches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/issues" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"number":9411,"title":"bump otel","html_url":"https://github.com/kubernetes/autoscaler/pull/9411","state":"open","repository_url":"https://api.github.com/repos/kubernetes/autoscaler","pull_request":{"url":"x"}}]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL)
	got, err := c.Search(context.Background(), "tok", "repo:kubernetes/autoscaler otel")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !strings.Contains(got, "#9411") || !strings.Contains(got, "bump otel") {
		t.Fatalf("search summary missing match: %q", got)
	}
}
