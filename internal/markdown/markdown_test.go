package markdown

import (
	"strings"
	"testing"
)

func TestRendersCommonMarkdown(t *testing.T) {
	got := string(ToSafeHTML("# Title\n\n**bold** and `code`\n\n- one\n- two"))
	for _, want := range []string{"<h1>", "<strong>bold</strong>", "<code>code</code>", "<ul>", "<li>one</li>"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered HTML missing %q: %s", want, got)
		}
	}
}

func TestSanitizesDangerousContent(t *testing.T) {
	cases := []string{
		"<script>alert(1)</script>",
		"<img src=x onerror=alert(1)>",
		`<iframe src="https://evil"></iframe>`,
		"<svg onload=alert(1)>",
		"[click](javascript:alert(1))",
		`<a href="javascript:alert(1)">x</a>`,
	}
	// Raw HTML is escaped by goldmark (no WithUnsafe) and bluemonday strips unsafe
	// schemes, so no real dangerous tag or javascript: href may survive.
	bad := []string{"<script", "<iframe", "<svg", "<img", `href="javascript`}
	for _, in := range cases {
		out := strings.ToLower(string(ToSafeHTML(in)))
		for _, b := range bad {
			if strings.Contains(out, strings.ToLower(b)) {
				t.Errorf("dangerous %q survived for input %q: %s", b, in, out)
			}
		}
	}
}

func TestExternalLinkGetsSafeRel(t *testing.T) {
	got := string(ToSafeHTML("[gh](https://github.com/kubernetes/autoscaler)"))
	if !strings.Contains(got, `href="https://github.com/kubernetes/autoscaler"`) {
		t.Fatalf("external link href missing: %s", got)
	}
	if !strings.Contains(got, `target="_blank"`) || !strings.Contains(got, "noopener") {
		t.Errorf("external link should open in a new tab with noopener: %s", got)
	}
}
