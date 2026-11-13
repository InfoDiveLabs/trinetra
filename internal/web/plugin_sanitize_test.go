package web

import (
	"strings"
	"testing"
)

func TestSanitizeEscapesHTML(t *testing.T) {
	got := string(sanitizeText(`<script>alert(1)</script>`))
	if strings.Contains(got, "<script>") || !strings.Contains(got, "&lt;script&gt;") {
		t.Fatalf("not escaped: %s", got)
	}
}

func TestSanitizeLinksHTTPSOnly(t *testing.T) {
	for _, in := range []string{"[x](javascript:alert(1))", "[x](http://a.example)", "[x](data:text/html,hi)"} {
		got := string(sanitizeText(in))
		if strings.Contains(got, "<a ") || strings.Contains(got, "href") {
			t.Errorf("%q produced a link: %s", in, got)
		}
		if !strings.Contains(got, "x") {
			t.Errorf("%q lost its text: %s", in, got)
		}
	}
	got := string(sanitizeText("[docs](https://example.com/a)"))
	for _, want := range []string{`href="https://example.com/a"`, `rel="noopener noreferrer nofollow"`, `target="_blank"`, ">docs</a>"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
}

func TestSanitizeNoAttributeInjection(t *testing.T) {
	got := string(sanitizeText(`[x](https://a" onmouseover="b)`))
	if strings.Contains(got, `onmouseover="b`) {
		t.Fatalf("attribute injected: %s", got)
	}
}

func TestSanitizeLiteSubset(t *testing.T) {
	got := string(sanitizeText("hello **bold** and *em* and `code`\n\n- a\n- b"))
	for _, want := range []string{"<p>", "<strong>bold</strong>", "<em>em</em>", "<code>code</code>", "<ul>", "<li>a</li>", "<li>b</li>"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
}
