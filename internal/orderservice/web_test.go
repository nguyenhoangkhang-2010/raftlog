package orderservice

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func TestTheScreensAreServedWithTheRightTypesAndASecurityPolicy(t *testing.T) {
	h := newHTTPRig(t)
	cases := []struct{ path, ctype string }{
		{"/", "text/html"}, {"/app.css", "text/css"}, {"/logic.js", "text/javascript"}, {"/app.js", "text/javascript"}, {"/favicon.svg", "image/svg+xml"},
	}
	for _, c := range cases {
		resp, err := http.Get(h.srv.URL + c.path)
		if err != nil {
			t.Fatal(err)
		}
		body := new(strings.Builder)
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			body.Write(buf[:n])
			if err != nil {
				break
			}
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), c.ctype) || body.Len() < 100 {
			t.Errorf("%s: %d %q, %d bytes", c.path, resp.StatusCode, resp.Header.Get("Content-Type"), body.Len())
		}
		if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
			t.Errorf("%s: content security policy %q", c.path, csp)
		}
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("X-Frame-Options") != "DENY" || resp.Header.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: headers %v", c.path, resp.Header)
		}
		if resp.Header.Get("ETag") == "" || resp.Header.Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: caching headers %q %q", c.path, resp.Header.Get("ETag"), resp.Header.Get("Cache-Control"))
		}
	}
}

func TestAPageThatHasNotChangedIsAnswered304(t *testing.T) {
	h := newHTTPRig(t)
	first, err := http.Get(h.srv.URL + "/app.js")
	if err != nil {
		t.Fatal(err)
	}
	first.Body.Close()
	req, _ := http.NewRequest("GET", h.srv.URL+"/app.js", nil)
	req.Header.Set("If-None-Match", first.Header.Get("ETag"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("status %d for a page that has not changed", resp.StatusCode)
	}
	req.Header.Set("If-None-Match", `"something-else"`)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d for a different ETag", resp.StatusCode)
	}
}

func TestNothingElseIsServedAsAFile(t *testing.T) {
	h := newHTTPRig(t)
	for _, path := range []string{"/web/index.html", "/index.html", "/app.js.map", "/logic.test.js", "/../go.mod", "/static/", "/app.css/", "/%2e%2e/etc/passwd", "/.env",
		"/fonts/", "/fonts", "/fonts/missing.woff2", "/fonts/bricolage-latin.woff2/", "/fonts/README.md"} {
		resp, err := http.Get(h.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 && resp.StatusCode != 301 {
			t.Errorf("%s: status %d, want 404", path, resp.StatusCode)
		}
	}
	req := httptest.NewRequest("POST", "/", nil)
	rec := httptest.NewRecorder()
	NewAPI(h.svc, testUsers(t), nil).ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /: %d", rec.Code)
	}
}

func TestEveryAnswerOfTheServiceCarriesNosniff(t *testing.T) {
	h := newHTTPRig(t)
	for _, path := range []string{"/api/menu", "/healthz", "/api/orders", "/nothing-here"} {
		resp, err := http.Get(h.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: no nosniff header", path)
		}
	}
}

func readEmbedded(t *testing.T, name string) string {
	t.Helper()
	data, err := webFS.ReadFile("web/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestThePageLoadsOnlyItsOwnFilesAndHasNoInlineCode(t *testing.T) {
	html := readEmbedded(t, "index.html")
	for _, m := range regexp.MustCompile(`(?:src|href)="([^"]*)"`).FindAllStringSubmatch(html, -1) {
		if !strings.HasPrefix(m[1], "/") || strings.HasPrefix(m[1], "//") {
			t.Errorf("the page refers to %q, which is not one of its own files", m[1])
		}
		if _, ok := assets[m[1]]; !ok {
			t.Errorf("the page refers to %q, which the service does not serve", m[1])
		}
	}
	for _, m := range regexp.MustCompile(`<script[^>]*>([\s\S]*?)</script>`).FindAllStringSubmatch(html, -1) {
		if strings.TrimSpace(m[1]) != "" {
			t.Errorf("an inline script: %.60q", m[1])
		}
	}
	if regexp.MustCompile(`\sstyle\s*=|\son[a-z]+\s*=|<style`).MatchString(html) {
		t.Error("the page has an inline style or an event attribute: the security policy would block it")
	}
	if !strings.Contains(html, `src="/logic.js"`) || strings.Index(html, `src="/logic.js"`) > strings.Index(html, `src="/app.js"`) {
		t.Error("logic.js must be loaded before app.js")
	}
}

func TestTheStylesheetUsesOnlyFontsTheServiceServes(t *testing.T) {
	css := readEmbedded(t, "app.css")
	urls := regexp.MustCompile(`url\(\s*["']?([^)"']+)["']?\s*\)`).FindAllStringSubmatch(css, -1)
	if len(urls) == 0 {
		t.Fatal("the stylesheet loads no font: the screens would fall back to whatever the device has")
	}
	for _, m := range urls {
		a, ok := assets[m[1]]
		if !ok {
			t.Errorf("the stylesheet refers to %q, which the service does not serve", m[1])
			continue
		}
		if a.contentType != "font/woff2" || !strings.HasPrefix(string(a.body), "wOF2") {
			t.Errorf("%s is %q and does not start like a woff2 file", m[1], a.contentType)
		}
	}
	if regexp.MustCompile(`@import|url\(\s*["']?(https?:)?//`).MatchString(css) {
		t.Error("the stylesheet loads something from another host")
	}
	if !strings.Contains(contentSecurityPolicy, "font-src 'self'") {
		t.Error("the security policy does not let the page load its own fonts")
	}
	h := newHTTPRig(t)
	resp, err := http.Get(h.srv.URL + urls[0][1])
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "font/woff2" {
		t.Errorf("%s: %d %q", urls[0][1], resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

func TestTheScriptsPutDataOnThePageAsTextNeverAsHTML(t *testing.T) {
	// A note that a customer types is shown on the kitchen's screen. Setting it as HTML would let
	// them run script there; the policy would stop it, and this keeps the code from trying.
	banned := regexp.MustCompile(`innerHTML|outerHTML|insertAdjacentHTML|document\.write|\beval\s*\(|new\s+Function|setAttribute\(\s*["']style|srcdoc|javascript:`)
	for _, name := range []string{"app.js", "logic.js"} {
		src := readEmbedded(t, name)
		if m := banned.FindString(src); m != "" {
			t.Errorf("%s uses %q", name, m)
		}
	}
}

func TestTheScreensLogicPassesItsOwnTests(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the tests of web/logic.js are not run")
	}
	if _, err := os.Stat("web/logic.test.js"); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "--test", "web/logic.test.js").CombinedOutput()
	if err != nil {
		t.Fatalf("the tests of web/logic.js failed: %v\n%s", err, out)
	}
}
