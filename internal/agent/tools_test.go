package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackfrancis/zumble-zay/internal/github"
)

func TestGitHubToolBoxInvoke(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/repos/octo/repo/contents/"):
			_, _ = w.Write([]byte(`{"type":"file","encoding":"base64","content":"aGk="}`)) // "hi"
		case strings.HasPrefix(r.URL.Path, "/repos/octo/repo/pulls/"):
			_, _ = w.Write([]byte(`{"number":7,"state":"open","merged":false,"title":"x","base":{"ref":"main"},"head":{"ref":"y"}}`))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	box := newGitHubToolBox(github.NewClient(srv.Client(), srv.URL), "tok", "octo/repo")

	if len(box.Definitions()) != 4 {
		t.Fatalf("want 4 tool defs, got %d", len(box.Definitions()))
	}

	// read_file with no repo argument falls back to the default (item) repo.
	out, err := box.Invoke(context.Background(), "github_read_file", json.RawMessage(`{"path":"go.mod"}`))
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if out != "hi" {
		t.Fatalf("read_file out = %q, want hi", out)
	}

	// get_pull_request routes to the pulls endpoint.
	if _, err := box.Invoke(context.Background(), "github_get_pull_request", json.RawMessage(`{"number":7}`)); err != nil {
		t.Fatalf("get_pull_request: %v", err)
	}

	// Unknown tool and missing required args error.
	if _, err := box.Invoke(context.Background(), "nope", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected error for unknown tool")
	}
	if _, err := box.Invoke(context.Background(), "github_read_file", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected error for missing path")
	}

	if len(paths) < 2 || !strings.HasPrefix(paths[0], "/repos/octo/repo/contents/go.mod") {
		t.Fatalf("repo default not applied; paths=%v", paths)
	}
}
