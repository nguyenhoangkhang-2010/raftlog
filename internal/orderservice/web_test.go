package orderservice

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/accounts"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/catalog"
	staffing "github.com/nguyenhoangkhang-2010/raftlog/internal/staff"
)

func TestTheScreensAreServedWithTheRightTypesAndASecurityPolicy(t *testing.T) {
	h := newHTTPRig(t)
	cases := []struct{ path, ctype string }{
		{"/", "text/html"}, {"/app.css", "text/css"}, {"/logic.js", "text/javascript"}, {"/admin.js", "text/javascript"}, {"/warehouse.js", "text/javascript"}, {"/shifts.js", "text/javascript"}, {"/staff.js", "text/javascript"}, {"/reports.js", "text/javascript"}, {"/board", "text/html"}, {"/board.js", "text/javascript"}, {"/app.js", "text/javascript"}, {"/favicon.svg", "image/svg+xml"},
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
	NewAPI(h.svc, TokenAuth(testUsers(t)), nil).ServeHTTP(rec, req)
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
	// app.js builds the screens of the managers from what admin.js defines, at once when it loads.
	for _, script := range []string{"admin.js", "warehouse.js", "shifts.js", "staff.js", "reports.js"} {
		if !strings.Contains(html, `src="/`+script+`"`) || strings.Index(html, `src="/`+script+`"`) > strings.Index(html, `src="/app.js"`) {
			t.Error(script + " must be loaded before app.js")
		}
	}
}

func TestEveryFixedMessageOfTheCatalogIsInTheVietnameseDictionary(t *testing.T) {
	dict := readEmbedded(t, "vi.js")
	var found []string
	scan := func(path string, patterns ...*regexp.Regexp) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, re := range patterns {
			for _, m := range re.FindAllStringSubmatch(string(data), -1) {
				found = append(found, m[1])
			}
		}
	}
	scan("../catalog/catalog.go", regexp.MustCompile(`InvalidError\{"([^"%\\]+)"\}`))
	scan("http_catalog.go", regexp.MustCompile(`InvalidError\{Reason: "([^"%\\]+)"\}`), regexp.MustCompile(`(?m)^\s*body\.Error = "([^"\\]+)"`))
	// the messages that are built with a number are got by asking for them
	for _, data := range [][]byte{{}, bytes.Repeat([]byte{1}, catalog.MaxPhotoBytes+1), pngBytes(8, 8)} {
		var inv *catalog.InvalidError
		if _, err := catalog.CheckImage(data); errors.As(err, &inv) {
			found = append(found, inv.Reason)
		}
	}
	// "say who saves" and the kind of document are the service's own business; no person is shown them
	skip := map[string]bool{"say who saves": true}
	checked := 0
	for _, msg := range found {
		if skip[msg] {
			continue
		}
		checked++
		if !strings.Contains(dict, `"`+msg+`":`) {
			t.Errorf("the message %q of the service is not in the server section of vi.js", msg)
		}
	}
	if checked < 10 {
		t.Fatalf("only %d messages were found in the sources: the patterns no longer match what is written", checked)
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
	for _, name := range []string{"app.js", "logic.js", "admin.js", "warehouse.js", "shifts.js", "staff.js", "reports.js", "board.js"} {
		src := readEmbedded(t, name)
		if m := banned.FindString(src); m != "" {
			t.Errorf("%s uses %q", name, m)
		}
	}
}

func TestEveryFixedMessageOfTheAccountsIsInTheVietnameseDictionary(t *testing.T) {
	// A message that the service writes in English and a screen shows in Vietnamese must be in the
	// dictionary, as written. The messages are read from the sources that write them, so that
	// changing a sentence there without changing the dictionary fails here.
	dict := readEmbedded(t, "vi.js")
	var found []string
	scan := func(path string, patterns ...*regexp.Regexp) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, re := range patterns {
			for _, m := range re.FindAllStringSubmatch(string(data), -1) {
				found = append(found, m[1])
			}
		}
	}
	invalid := regexp.MustCompile(`invalid\("([^"%\\]+)"\)`)
	password := regexp.MustCompile(`PasswordError\{"([^"\\]+)"\}`)
	sentinel := regexp.MustCompile(`errors\.New\("accounts: ([^"\\]+)"\)`)
	files, _ := filepath.Glob("../accounts/*.go")
	for _, f := range files {
		if !strings.HasSuffix(f, "_test.go") {
			scan(f, invalid, password, sentinel)
		}
	}
	scan("http_accounts.go", regexp.MustCompile(`InvalidError\{Reason: "([^"\\]+)"\}`))
	// the messages that are built with a number are got by asking for them
	for _, pw := range []string{"short", strings.Repeat("x1y2z3", 30)} {
		var pe *accounts.PasswordError
		if err := accounts.CheckPassword(pw, "an-id", ""); errors.As(err, &pe) {
			found = append(found, pe.Reason)
		}
	}
	// two sentinels are not shown to a person: a wrong login is told by its code, and the stored
	// hash is the service's own problem
	skip := map[string]bool{"wrong login name or password": true, "no such session": true, "the stored password hash is not an argon2id hash this program understands": true}
	checked := 0
	for _, msg := range found {
		if skip[msg] {
			continue
		}
		checked++
		if !strings.Contains(dict, `"`+msg+`":`) {
			t.Errorf("the message %q of the service is not in the server section of vi.js", msg)
		}
	}
	if checked < 20 {
		t.Fatalf("only %d messages were found in the sources: the patterns no longer match what is written", checked)
	}
}

func TestEveryFixedMessageOfTheWarehouseIsInTheVietnameseDictionary(t *testing.T) {
	dict := readEmbedded(t, "vi.js")
	var found []string
	scan := func(path string, patterns ...*regexp.Regexp) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, re := range patterns {
			for _, m := range re.FindAllStringSubmatch(string(data), -1) {
				found = append(found, m[1])
			}
		}
	}
	scan("../inventory/inventory.go", regexp.MustCompile(`invalid\("([^"%\\]+)"\)`), regexp.MustCompile(`errors\.New\("inventory: ([^"\\]+)"\)`))
	scan("http_inventory.go", regexp.MustCompile(`InvalidError\{Reason: "([^"%\\]+)"\}`))
	scan("inventory.go", regexp.MustCompile(`badStock\("([^"%\\]+)"\)`))
	scan("../kitchen/stock/stock.go", regexp.MustCompile(`errors\.New\("stock: ([^"\\]+)"\)`))
	// "say who saves" is the service's own business; no person is shown it
	skip := map[string]bool{"say who saves": true, "unknown kind of movement": true, "a count cannot be negative": true}
	checked := 0
	for _, msg := range found {
		if skip[msg] {
			continue
		}
		checked++
		if !strings.Contains(dict, `"`+msg+`":`) && !strings.Contains(dict, `"stock: `+msg+`":`) {
			t.Errorf("the message %q of the service is not in the server section of vi.js", msg)
		}
	}
	if checked < 15 {
		t.Fatalf("only %d messages were found in the sources: the patterns no longer match what is written", checked)
	}
}

func TestEveryFixedMessageOfTheShiftsIsInTheVietnameseDictionary(t *testing.T) {
	dict := readEmbedded(t, "vi.js")
	var found []string
	scan := func(path string, patterns ...*regexp.Regexp) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, re := range patterns {
			for _, m := range re.FindAllStringSubmatch(string(data), -1) {
				found = append(found, m[1])
			}
		}
	}
	scan("../shifts/shifts.go", regexp.MustCompile(`invalid\("([^"%\\]+)"\)`), regexp.MustCompile(`errors\.New\("shifts: ([^"\\]+)"\)`))
	scan("http_shifts.go", regexp.MustCompile(`InvalidError\{Reason: "([^"%\\]+)"\}`))
	// the messages that are built from a field name are got by writing them out
	found = append(found, "the note is at most 300 characters", "the reason is at most 200 characters")
	// "say who ..." is the service's own business; no person is shown it
	checked := 0
	for _, msg := range found {
		if strings.HasPrefix(msg, "say who") {
			continue
		}
		checked++
		if !strings.Contains(dict, `"`+msg+`":`) {
			t.Errorf("the message %q of the service is not in the server section of vi.js", msg)
		}
	}
	if checked < 12 {
		t.Fatalf("only %d messages were found in the sources: the patterns no longer match what is written", checked)
	}
}

func TestEveryFixedMessageOfTheExpensesIsInTheVietnameseDictionary(t *testing.T) {
	dict := readEmbedded(t, "vi.js")
	var found []string
	scan := func(path string, patterns ...*regexp.Regexp) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, re := range patterns {
			for _, m := range re.FindAllStringSubmatch(string(data), -1) {
				found = append(found, m[1])
			}
		}
	}
	scan("../expenses/expenses.go", regexp.MustCompile(`invalid\("([^"%\\]+)"\)`), regexp.MustCompile(`errors\.New\("expenses: ([^"\\]+)"\)`))
	scan("http_costs.go", regexp.MustCompile(`InvalidError\{Reason: "([^"%\\]+)"\}`))
	// the messages that are built from a field name are got by writing them out
	found = append(found, "the category is 1 to 40 characters", "the note is at most 200 characters", "the reason is at most 200 characters")
	checked := 0
	for _, msg := range found {
		if strings.HasPrefix(msg, "say who") || msg == "not found" {
			continue
		}
		checked++
		if !strings.Contains(dict, `"`+msg+`":`) {
			t.Errorf("the message %q of the service is not in the server section of vi.js", msg)
		}
	}
	if checked < 6 {
		t.Fatalf("only %d messages were found in the sources: the patterns no longer match what is written", checked)
	}
}

func TestEveryFixedMessageOfTheStaffIsInTheVietnameseDictionary(t *testing.T) {
	dict := readEmbedded(t, "vi.js")
	var found []string
	scan := func(path string, patterns ...*regexp.Regexp) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, re := range patterns {
			for _, m := range re.FindAllStringSubmatch(string(data), -1) {
				found = append(found, m[1])
			}
		}
	}
	scan("../staff/staff.go", regexp.MustCompile(`invalid\("([^"%\\]+)"\)`))
	scan("http_staff.go", regexp.MustCompile(`InvalidError\{Reason: "([^"%\\]+)"\}`))
	// the messages that are built from a field name or a number are got by writing them out; the
	// sentinels (overlap, already clocked in, ...) are told by their codes, which the screens know
	for _, what := range []string{"the day", "the first day", "the last day"} {
		found = append(found, what+" is not a date like 2026-11-14")
	}
	found = append(found, "the station is at most 20 characters", "the note is at most 200 characters", "the reason is at most 200 characters",
		fmt.Sprintf("a rate is a whole number of đồng for an hour, up to %d", staffing.MaxRate), "a shift is a number from 1", "an entry is a number from 1")
	checked := 0
	for _, msg := range found {
		if strings.HasPrefix(msg, "say who") {
			continue
		}
		checked++
		if !strings.Contains(dict, `"`+msg+`":`) {
			t.Errorf("the message %q of the service is not in the server section of vi.js", msg)
		}
	}
	if checked < 20 {
		t.Fatalf("only %d messages were found in the sources: the patterns no longer match what is written", checked)
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
	out, err := exec.Command(node, "--test", "web/logic.test.js", "web/i18n.test.js", "web/admin.test.js", "web/warehouse.test.js", "web/shifts.test.js", "web/staff.test.js", "web/reports.test.js", "web/board.test.js").CombinedOutput()
	if err != nil {
		t.Fatalf("the tests of web/logic.js failed: %v\n%s", err, out)
	}
}
