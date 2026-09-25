package web

import (
	"io/fs"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Trinetra brand palette (see the rename design's global constraints).
const (
	brandInk       = "#0E1114"
	brandGraphite  = "#1C2127"
	brandSlate     = "#8A949E"
	brandAsh       = "#ECE8E1"
	brandEmber     = "#FF5B1F"
	brandVerdigris = "#3FBFA6"
	brandAmber     = "#F2B23A"
)

// TestBrandAssetsServed pins that the copied brand kit files and the embedded
// Outfit font are served from the embedded asset tree with a deterministic
// Content-Type (not the host's mime.types).
func TestBrandAssetsServed(t *testing.T) {
	h := newHandler(enrollTestDeps(t))
	cases := map[string]string{
		"/assets/brand/favicon.ico":                    "image/x-icon",
		"/assets/brand/icon-32.png":                    "image/png",
		"/assets/brand/icon-180.png":                   "image/png",
		"/assets/brand/icon-192.png":                   "image/png",
		"/assets/brand/icon-512.png":                   "image/png",
		"/assets/brand/trinetra-wordmark-ash.png":      "image/png",
		"/assets/brand/trinetra-wordmark-ink.png":      "image/png",
		"/assets/fonts/outfit-latin-wght-normal.woff2": "font/woff2",
		"/assets/fonts/OFL.txt":                        "text/plain; charset=utf-8",
	}
	for path, wantCT := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); ct != wantCT {
			t.Errorf("GET %s Content-Type = %q, want %q", path, ct, wantCT)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("GET %s returned an empty body", path)
		}
	}
}

// brandPages renders the four branded surfaces: the signed-in app shell
// (sidebar), login, enroll and the anonymous public status page.
func brandPages(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}

	d := enrollTestDeps(t)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	get := func(name string, req *http.Request, hh http.Handler) {
		rec := httptest.NewRecorder()
		hh.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d, want 200", name, rec.Code)
		}
		out[name] = rec.Body.String()
	}
	get("dashboard", seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/"), h)
	get("login", httptest.NewRequest(http.MethodGet, "/login", nil), h)
	get("enroll", httptest.NewRequest(http.MethodGet, "/enroll", nil), h)

	pd, cfg, _ := configTestDeps(t)
	(*cfg).Public.Enabled = true
	(*cfg).Public.Panels = []string{"cpu"}
	pd.Snapshot = func() DashboardView { return publicTestSnapshot() }
	get("public", httptest.NewRequest(http.MethodGet, "/", nil), newHandler(pd))
	return out
}

// TestPagesCarryWordmarkAndFavicons pins the visual identity on every
// branded surface: both theme variants of the wordmark (ash for dark, ink
// for light) with alt="trinetra", the old .lamp dot gone, and the favicon /
// apple-touch-icon links in the head -- without a web manifest.
func TestPagesCarryWordmarkAndFavicons(t *testing.T) {
	for name, body := range brandPages(t) {
		for _, want := range []string{
			`src="/assets/brand/trinetra-wordmark-ash.png?v=` + assetVersion + `"`,
			`src="/assets/brand/trinetra-wordmark-ink.png?v=` + assetVersion + `"`,
			`alt="trinetra"`,
			`class="wordmark wm-ash"`,
			`class="wordmark wm-ink"`,
			`rel="icon" href="/assets/brand/favicon.ico?v=` + assetVersion + `"`,
			`rel="icon" type="image/png" sizes="32x32" href="/assets/brand/icon-32.png?v=` + assetVersion + `"`,
			`rel="apple-touch-icon" href="/assets/brand/icon-180.png?v=` + assetVersion + `"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s page missing %s", name, want)
			}
		}
		if strings.Contains(body, `class="lamp"`) {
			t.Errorf("%s page still renders the old .lamp brand dot", name)
		}
		if strings.Contains(body, `rel="manifest"`) {
			t.Errorf("%s page links a web manifest; the setup is manifest-free", name)
		}
	}
}

// externalAssetURL matches an absolute http(s) URL used to LOAD something: an
// HTML src/href attribute, a CSS url(...) or an @import. Plain text such as a
// form placeholder="https://ntfy.sh" is not an asset reference.
var externalAssetURL = regexp.MustCompile(`(?i)(\b(src|href|srcset)\s*=\s*["']?\s*https?://|url\(\s*["']?\s*https?://|@import\s+["']?\s*https?://)`)

// TestNoExternalAssetURLs pins "no runtime external requests": every
// template and stylesheet must load its fonts, images, scripts and styles
// from this server's embedded /assets/ tree.
func TestNoExternalAssetURLs(t *testing.T) {
	check := func(fsys fs.FS, root string, exts ...string) int {
		n := 0
		_ = fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			ok := false
			for _, e := range exts {
				ok = ok || strings.HasSuffix(p, e)
			}
			if !ok {
				return nil
			}
			n++
			b, err := fs.ReadFile(fsys, p)
			if err != nil {
				t.Fatalf("read %s: %v", p, err)
			}
			for _, m := range externalAssetURL.FindAllString(string(b), -1) {
				t.Errorf("%s loads an external asset: %q", p, m)
			}
			return nil
		})
		return n
	}
	if n := check(templatesFS, "templates", ".html"); n == 0 {
		t.Fatal("no templates scanned")
	}
	if n := check(assetsFS, "assets", ".css"); n == 0 {
		t.Fatal("no stylesheets scanned")
	}
	// The matcher itself must catch the shapes it claims to.
	for _, bad := range []string{`<img src="https://x/y.png">`, `@font-face{src:url(https://fonts.gstatic.com/a.woff2)}`, `@import "http://x/a.css";`, `<link href='https://x/a.css'>`} {
		if !externalAssetURL.MatchString(bad) {
			t.Errorf("matcher misses %q", bad)
		}
	}
}

// cssThemeVars parses the custom properties declared in style.css's
// `:root[data-theme="<theme>"]{...}` block.
func cssThemeVars(t *testing.T, css, theme string) map[string]string {
	t.Helper()
	sel := `:root[data-theme="` + theme + `"]{`
	i := strings.Index(css, sel)
	if i < 0 {
		t.Fatalf("style.css has no %s block", sel)
	}
	body := css[i+len(sel):]
	body = body[:strings.Index(body, "}")]
	vars := map[string]string{}
	for _, decl := range strings.Split(body, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(decl), ":")
		if ok && strings.HasPrefix(k, "--") {
			vars[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return vars
}

// relLum is the WCAG 2.x relative luminance of an opaque #RRGGBB colour.
func relLum(t *testing.T, hex string) float64 {
	t.Helper()
	if len(hex) != 7 || hex[0] != '#' {
		t.Fatalf("not an opaque #RRGGBB colour: %q", hex)
	}
	ch := func(s string) float64 {
		v, err := strconv.ParseUint(s, 16, 8)
		if err != nil {
			t.Fatalf("bad colour %q: %v", hex, err)
		}
		c := float64(v) / 255
		if c <= 0.04045 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*ch(hex[1:3]) + 0.7152*ch(hex[3:5]) + 0.0722*ch(hex[5:7])
}

func contrast(t *testing.T, a, b string) float64 {
	la, lb := relLum(t, a), relLum(t, b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func readStyleCSS(t *testing.T) string {
	t.Helper()
	b, err := assetsFS.ReadFile("assets/style.css")
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	return string(b)
}

// TestStyleCSSUsesBrandPalette pins the token remap: ink/graphite/ash
// surfaces and text, ember for --signal/--crit, verdigris for --ok and amber
// for --warn in the dark theme; the light theme keeps pure ember for the one
// primary action (--signal) and ash as its page background.
func TestStyleCSSUsesBrandPalette(t *testing.T) {
	css := readStyleCSS(t)
	dark := cssThemeVars(t, css, "dark")
	light := cssThemeVars(t, css, "light")
	want := []struct {
		theme map[string]string
		name  string
		k, v  string
	}{
		{dark, "dark", "--bg", brandInk},
		{dark, "dark", "--surface", brandGraphite},
		{dark, "dark", "--text", brandAsh},
		{dark, "dark", "--muted", brandSlate},
		{dark, "dark", "--signal", brandEmber},
		{dark, "dark", "--crit", brandEmber},
		{dark, "dark", "--ok", brandVerdigris},
		{dark, "dark", "--warn", brandAmber},
		{light, "light", "--bg", brandAsh},
		{light, "light", "--text", brandInk},
		{light, "light", "--signal", brandEmber},
	}
	for _, w := range want {
		if got := w.theme[w.k]; !strings.EqualFold(got, w.v) {
			t.Errorf("%s %s = %q, want %s", w.name, w.k, got, w.v)
		}
	}
}

// TestStyleCSSContrast checks WCAG contrast of the text tokens on every
// opaque surface in both themes: body text and --muted at least 4.5:1,
// --faint (eyebrows, table heads, axis labels) at least 3:1, and the status
// colours used as small badge/trend text at least 4.5:1.
func TestStyleCSSContrast(t *testing.T) {
	css := readStyleCSS(t)
	for _, theme := range []string{"dark", "light"} {
		v := cssThemeVars(t, css, theme)
		for _, surf := range []string{"--bg", "--surface", "--elev"} {
			for _, tc := range []struct {
				tok string
				min float64
			}{{"--text", 4.5}, {"--muted", 4.5}, {"--faint", 3.0}, {"--crit", 4.5}, {"--ok", 4.5}, {"--warn", 4.5}} {
				r := contrast(t, v[tc.tok], v[surf])
				t.Logf("%-5s %-7s on %-9s %s on %s = %.2f:1", theme, tc.tok, surf, v[tc.tok], v[surf], r)
				if r < tc.min {
					t.Errorf("%s: %s %s on %s %s = %.2f:1, want >= %.1f:1", theme, tc.tok, v[tc.tok], surf, v[surf], r, tc.min)
				}
			}
		}
		// The primary button draws its label on --signal; the mobile alert
		// count badge draws its number on --crit.
		for _, p := range [][2]string{{"--on-signal", "--signal"}, {"--on-crit", "--crit"}} {
			r := contrast(t, v[p[0]], v[p[1]])
			t.Logf("%-5s %s %s on %s %s = %.2f:1", theme, p[0], v[p[0]], p[1], v[p[1]], r)
			if r < 4.5 {
				t.Errorf("%s: %s on %s = %.2f:1, want >= 4.5:1", theme, p[0], p[1], r)
			}
		}
	}
}

// TestStyleCSSEmbedsOutfitForHeadings pins the display face: Outfit is
// declared via @font-face from the embedded woff2 and used for headings only
// (body text keeps --font-sans).
func TestStyleCSSEmbedsOutfitForHeadings(t *testing.T) {
	css := readStyleCSS(t)
	for _, want := range []string{
		`@font-face{font-family:"Outfit"`,
		`url(/assets/fonts/outfit-latin-wght-normal.woff2) format("woff2")`,
		`--font-display:"Outfit",`,
		`h1,h2,h3{font-family:var(--font-display)`,
	} {
		if !strings.Contains(css, want) {
			t.Errorf("style.css missing %s", want)
		}
	}
	if strings.Contains(css, "body{background:var(--bg); color:var(--text); font-family:var(--font-display)") {
		t.Error("body text must not use the display face")
	}
}
