package orderservice

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/accounts"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/pgmigrate"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/pgtest"
)

var ctxBackground = context.Background()

// goodPassword is a password of the tests; nothing else in the repository is signed in with it.
const goodPassword = "correct horse battery"

type accRig struct {
	*rig
	acc  *accounts.Service
	pool *pgxpool.Pool
	srv  *httptest.Server
}

func newAccRig(t *testing.T, opts AccountsOptions) *accRig {
	t.Helper()
	pool := pgtest.Pool(t)
	if _, err := pgmigrate.Apply(ctxBackground, pool, accounts.Migrations()); err != nil {
		t.Fatal(err)
	}
	acc, err := accounts.New(pool, accounts.Config{Hash: accounts.HashParams{Memory: 8, Iterations: 1, Parallelism: 1}})
	if err != nil {
		t.Fatal(err)
	}
	r := newRig(t)
	srv := httptest.NewServer(NewAPI(r.svc, nil, nil, WithAccounts(acc, opts)))
	t.Cleanup(srv.Close)
	return &accRig{rig: r, acc: acc, pool: pool, srv: srv}
}

func (a *accRig) user(id, station string, roles ...rbac.Role) {
	a.t.Helper()
	if _, err := a.acc.CreateWithPassword(ctxBackground, "test", accounts.NewUser{ID: id, DisplayName: "Person " + id, Roles: roles, Station: station}, goodPassword); err != nil {
		a.t.Fatalf("create %s: %v", id, err)
	}
}

// browser is a client with a cookie jar, that sends what a browser sends with a request that
// the page itself makes.
type browser struct {
	t      *testing.T
	base   string
	client *http.Client
	jar    *cookiejar.Jar
	// site is the Sec-Fetch-Site of requests that change something ("" sends none, like a
	// script outside a browser); origin, when set, is sent as the Origin header.
	site, origin string
}

func (a *accRig) browser() *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: a.t, base: a.srv.URL, client: &http.Client{Jar: jar, Timeout: 10 * time.Second}, jar: jar, site: "same-origin"}
}

func (b *browser) request(method, path string, body any) *http.Request {
	var rd io.Reader
	switch v := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(v)
	default:
		raw, _ := json.Marshal(v)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, b.base+path, rd)
	if err != nil {
		b.t.Fatal(err)
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != "GET" && method != "HEAD" {
		if b.site != "" {
			req.Header.Set("Sec-Fetch-Site", b.site)
		}
		if b.origin != "" {
			req.Header.Set("Origin", b.origin)
		}
	}
	return req
}

func (b *browser) do(method, path string, body, out any) (int, http.Header) {
	b.t.Helper()
	resp, err := b.client.Do(b.request(method, path, body))
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			b.t.Fatalf("%s %s: %q is not the expected JSON: %v", method, path, raw, err)
		}
	}
	return resp.StatusCode, resp.Header
}

func (b *browser) status(method, path string, body any) int {
	b.t.Helper()
	code, _ := b.do(method, path, body, nil)
	return code
}

func (b *browser) signIn(id string) {
	b.t.Helper()
	if code := b.status("POST", "/api/login", map[string]string{"username": id, "password": goodPassword}); code != 200 {
		b.t.Fatalf("sign in as %s: %d", id, code)
	}
}

func (b *browser) cookie(name string) *http.Cookie {
	u, _ := url.Parse(b.base)
	for _, c := range b.jar.Cookies(u) {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestTheScreenIsToldHowPeopleSignInHere(t *testing.T) {
	a := newAccRig(t, AccountsOptions{})
	var m struct{ Mode string }
	if code, _ := a.browser().do("GET", "/api/auth", nil, &m); code != 200 || m.Mode != "password" {
		t.Fatalf("/api/auth: %d %+v", code, m)
	}
	h := newHTTPRig(t) // the service with tokens says so too
	var tm struct{ Mode string }
	if code := h.do("GET", "/api/auth", "", nil, &tm); code != 200 || tm.Mode != "token" {
		t.Fatalf("/api/auth with tokens: %d %+v", code, tm)
	}
	if code := h.do("POST", "/api/login", "", `{"username":"a","password":"b"}`, nil); code == 200 {
		t.Fatal("a service with tokens answers to a sign-in")
	}
}

func TestSigningInSetsACookieThatScriptCannotReadAndPutsNoTokenInTheAnswer(t *testing.T) {
	a := newAccRig(t, AccountsOptions{MaxLifetime: 48 * time.Hour})
	a.user("thu-ngan-1", "", rbac.Cashier)
	b := a.browser()
	req := b.request("POST", "/api/login", map[string]string{"username": "thu-ngan-1", "password": goodPassword})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("sign in: %d %s", resp.StatusCode, raw)
	}
	var c *http.Cookie
	for _, x := range resp.Cookies() {
		if x.Name == "kitchen_session" {
			c = x
		}
	}
	if c == nil {
		t.Fatal("no session cookie")
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.MaxAge != 48*3600 {
		t.Errorf("cookie %+v: want HttpOnly, SameSite=Strict, Path=/ and the lifetime of a session", c)
	}
	if c.Secure {
		t.Error("the cookie is Secure although the request was plain HTTP: a browser would not return it")
	}
	if len(c.Value) != 43 || bytes.Contains(raw, []byte(c.Value)) {
		t.Errorf("the token is in the answer, or is not what was expected: %s", raw)
	}
	var me struct {
		ID                 string          `json:"id"`
		Roles              []string        `json:"roles"`
		Perm               map[string]bool `json:"permissions"`
		MustChangePassword bool            `json:"must_change_password"`
		Expires            time.Time       `json:"session_expires_at"`
	}
	if err := json.Unmarshal(raw, &me); err != nil || me.ID != "thu-ngan-1" || !me.Perm["order.record_payment"] || me.Perm["user.manage"] || me.Expires.IsZero() {
		t.Fatalf("the answer to a sign-in is %s", raw)
	}
}

func TestTheCookieIsSecureBehindAProxyThatDoesTLS(t *testing.T) {
	a := newAccRig(t, AccountsOptions{SecureCookies: true})
	a.user("thu-ngan-1", "", rbac.Cashier)
	b := a.browser()
	_, hdr := b.do("POST", "/api/login", map[string]string{"username": "thu-ngan-1", "password": goodPassword}, nil)
	if sc := hdr.Get("Set-Cookie"); !strings.Contains(sc, "Secure") || !strings.Contains(sc, "HttpOnly") || !strings.Contains(sc, "SameSite=Strict") {
		t.Fatalf("Set-Cookie: %q", sc)
	}
}

func TestAWrongPasswordAndAnUnknownNameAreAnsweredAlike(t *testing.T) {
	a := newAccRig(t, AccountsOptions{})
	a.user("thu-ngan-1", "", rbac.Cashier)
	b := a.browser()
	var e1, e2 errAnswer
	c1, h1 := b.do("POST", "/api/login", map[string]string{"username": "thu-ngan-1", "password": "not the password"}, &e1)
	c2, h2 := b.do("POST", "/api/login", map[string]string{"username": "nobody-at-all", "password": "not the password"}, &e2)
	if c1 != 401 || c2 != 401 || e1.Error != e2.Error || e1.Code != e2.Code || e1.Code != "bad_credentials" {
		t.Fatalf("answers differ: %d %+v and %d %+v", c1, e1, c2, e2)
	}
	if h1.Get("Set-Cookie") != "" || h2.Get("Set-Cookie") != "" {
		t.Fatal("a failed sign-in set a cookie")
	}
}

func TestEveryRouteNeedsASessionAndABearerTokenIsNotOne(t *testing.T) {
	a := newAccRig(t, AccountsOptions{})
	a.user("thu-ngan-1", "", rbac.Cashier)
	b := a.browser()
	paths := []string{"/api/orders", "/api/stock", "/api/me", "/api/stream", "/api/me/sessions", "/api/admin/users", "/api/admin/events"}
	for _, p := range paths {
		var e errAnswer
		code, hdr := b.do("GET", p, nil, &e)
		if code != 401 || e.Code != "unauthorized" {
			t.Errorf("%s without a session: %d %+v", p, code, e)
		}
		if hdr.Get("WWW-Authenticate") != "" {
			t.Errorf("%s offers a bearer scheme that does not exist here", p)
		}
	}
	b.signIn("thu-ngan-1")
	token := b.cookie("kitchen_session").Value
	// The same token, presented as a bearer by a client with no cookie, is not accepted: a
	// session belongs to the browser it was given to.
	req, _ := http.NewRequest("GET", a.srv.URL+"/api/orders", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("a session token as a bearer: %d", resp.StatusCode)
	}
	if code := b.status("GET", "/api/orders", nil); code != 200 {
		t.Fatalf("with the cookie: %d", code)
	}
}

func TestRolesFromTheDatabaseDecideWhatEachSessionMayDo(t *testing.T) {
	a := newAccRig(t, AccountsOptions{})
	a.stockAll()
	a.user("cus-1", "", rbac.Customer)
	a.user("cash-1", "", rbac.Cashier)
	a.user("admin-1", "", rbac.Admin)
	cust, cash, adm := a.browser(), a.browser(), a.browser()
	cust.signIn("cus-1")
	cash.signIn("cash-1")
	adm.signIn("admin-1")

	var placed struct{ Order struct{ ID string } }
	if code, _ := cust.do("POST", "/api/orders", map[string]any{"command_id": "acc-order-0001", "items": []map[string]any{{"item": "toastie", "quantity": 1}}}, &placed); code != 201 {
		t.Fatalf("the customer's order: %d", code)
	}
	pay := map[string]any{"command_id": "acc-pay-00001", "method": "card", "reference": "slip-1"}
	if code := cust.status("POST", "/api/orders/"+placed.Order.ID+"/pay", pay); code != 403 {
		t.Fatalf("a customer paying: %d", code)
	}
	if code := cash.status("POST", "/api/orders/"+placed.Order.ID+"/pay", pay); code != 200 {
		t.Fatalf("the cashier paying: %d", code)
	}
	// The administrator manages accounts and touches no orders or money.
	if code := adm.status("POST", "/api/orders", map[string]any{"command_id": "acc-order-0002", "items": []map[string]any{{"item": "toastie", "quantity": 1}}}); code != 403 {
		t.Fatalf("an administrator placing an order: %d", code)
	}
	for _, c := range []*browser{cust, cash} {
		if code := c.status("GET", "/api/admin/users", nil); code != 403 {
			t.Fatalf("a person without user.manage reading the users: %d", code)
		}
	}
	if code := adm.status("GET", "/api/admin/users", nil); code != 200 {
		t.Fatalf("the administrator reading the users: %d", code)
	}
}

func TestChangingARoleCountsAtTheNextRequest(t *testing.T) {
	a := newAccRig(t, AccountsOptions{})
	a.user("cash-1", "", rbac.Cashier)
	a.user("admin-1", "", rbac.Admin)
	cash, adm := a.browser(), a.browser()
	cash.signIn("cash-1")
	adm.signIn("admin-1")
	if code := cash.status("GET", "/api/orders", nil); code != 200 {
		t.Fatal(code)
	}
	body := map[string]any{"display_name": "Cash One", "roles": []string{"customer"}}
	if code := adm.status("PUT", "/api/admin/users/cash-1", body); code != 200 {
		t.Fatalf("changing the role: %d", code)
	}
	var me struct {
		Perm map[string]bool `json:"permissions"`
	}
	cash.do("GET", "/api/me", nil, &me)
	if me.Perm["order.record_payment"] || !me.Perm["order.place"] {
		t.Fatalf("the permissions after the change: %v", me.Perm)
	}
}

func TestACreatedAccountMustChangeItsTemporaryPasswordBeforeAnythingElse(t *testing.T) {
	a := newAccRig(t, AccountsOptions{})
	a.user("admin-1", "", rbac.Admin)
	adm := a.browser()
	adm.signIn("admin-1")

	var created struct {
		User      userView `json:"user"`
		Temporary string   `json:"temporary_password"`
	}
	code, _ := adm.do("POST", "/api/admin/users", map[string]any{"id": "bep-do-uong", "display_name": "Bếp đồ uống", "roles": []string{"kitchen"}, "station": "drinks"}, &created)
	if code != 201 || created.Temporary == "" || !created.User.MustChangePassword || created.User.Station != "drinks" {
		t.Fatalf("create: %d %+v", code, created)
	}
	// The password is in that answer once; the list does not carry it, nor a hash.
	var raw map[string]json.RawMessage
	adm.do("GET", "/api/admin/users", nil, &raw)
	if bytes.Contains(raw["users"], []byte(created.Temporary)) || bytes.Contains(bytes.ToLower(raw["users"]), []byte("hash")) || bytes.Contains(bytes.ToLower(raw["users"]), []byte("\"password\"")) {
		t.Fatalf("the list of users carries a secret: %s", raw["users"])
	}

	cook := a.browser()
	if code := cook.status("POST", "/api/login", map[string]string{"username": "bep-do-uong", "password": created.Temporary}); code != 200 {
		t.Fatalf("sign in with the temporary password: %d", code)
	}
	var e errAnswer
	for _, p := range []string{"/api/orders", "/api/stock", "/api/stream", "/api/me/sessions"} {
		if code, _ := cook.do("GET", p, nil, &e); code != 403 || e.Code != "password_change_required" {
			t.Errorf("%s before the change: %d %+v", p, code, e)
		}
	}
	var me struct {
		Must bool `json:"must_change_password"`
	}
	if code, _ := cook.do("GET", "/api/me", nil, &me); code != 200 || !me.Must {
		t.Fatalf("/api/me must stay open to say what is wrong: %d %+v", code, me)
	}
	// A wrong current password is not a 401 (that would sign the person out of the page).
	if code, _ := cook.do("POST", "/api/me/password", map[string]string{"current": "not it at all", "new": "a long enough password"}, &e); code != 400 || e.Code != "wrong_password" {
		t.Fatalf("a wrong current password: %d %+v", code, e)
	}
	if code, _ := cook.do("POST", "/api/me/password", map[string]string{"current": created.Temporary, "new": "short"}, &e); code != 400 || e.Code != "weak_password" {
		t.Fatalf("a weak new password: %d %+v", code, e)
	}
	if code, _ := cook.do("POST", "/api/me/password", map[string]string{"current": created.Temporary, "new": "my own long password"}, &me); code != 200 || me.Must {
		t.Fatalf("the change: %d %+v", code, me)
	}
	if code := cook.status("GET", "/api/orders?station=drinks", nil); code != 200 {
		t.Fatalf("after the change: %d", code)
	}
}

func TestDisablingAnAccountEndsItsSessionAtOnce(t *testing.T) {
	a := newAccRig(t, AccountsOptions{})
	a.user("cash-1", "", rbac.Cashier)
	a.user("admin-1", "", rbac.Admin)
	cash, adm := a.browser(), a.browser()
	cash.signIn("cash-1")
	adm.signIn("admin-1")
	if code := cash.status("GET", "/api/orders", nil); code != 200 {
		t.Fatal(code)
	}
	if code := adm.status("POST", "/api/admin/users/cash-1/disable", nil); code != 200 {
		t.Fatalf("disable: %d", code)
	}
	if code := cash.status("GET", "/api/orders", nil); code != 401 {
		t.Fatalf("a disabled account's next request: %d", code)
	}
	var e errAnswer
	if code, _ := cash.do("POST", "/api/login", map[string]string{"username": "cash-1", "password": goodPassword}, &e); code != 401 || e.Code != "bad_credentials" {
		t.Fatalf("signing in to a disabled account: %d %+v", code, e)
	}
}

func TestAnAdministratorCannotLockThemselvesOut(t *testing.T) {
	a := newAccRig(t, AccountsOptions{})
	a.user("admin-1", "", rbac.Admin)
	adm := a.browser()
	adm.signIn("admin-1")
	var e errAnswer
	if code, _ := adm.do("POST", "/api/admin/users/admin-1/disable", nil, &e); code != 409 || e.Code != "conflict" {
		t.Fatalf("disabling yourself: %d %+v", code, e)
	}
	if code, _ := adm.do("PUT", "/api/admin/users/admin-1", map[string]any{"display_name": "x", "roles": []string{"cashier"}}, &e); code != 409 {
		t.Fatalf("taking your own admin role: %d", code)
	}
	if code, _ := adm.do("POST", "/api/admin/users/admin-1/reset-password", nil, &e); code != 409 {
		t.Fatalf("resetting your own password: %d", code)
	}
	if code, _ := adm.do("GET", "/api/admin/users/nobody-here/sessions", nil, &e); code != 200 {
		t.Fatalf("the sessions of an unknown account are an empty list, not an error: %d", code)
	}
	if code, _ := adm.do("POST", "/api/admin/users/nobody-here/unlock", nil, &e); code != 404 {
		t.Fatalf("unlocking an unknown account: %d", code)
	}
	if code, _ := adm.do("POST", "/api/admin/users", map[string]any{"id": "x", "display_name": "x", "roles": []string{"cashier"}}, &e); code != 400 || e.Code != "invalid" {
		t.Fatalf("a bad account: %d %+v", code, e)
	}
	if code, _ := adm.do("POST", "/api/admin/users", map[string]any{"id": "admin-1", "display_name": "x", "roles": []string{"cashier"}}, &e); code != 409 {
		t.Fatalf("a name that is taken: %d", code)
	}
	if code, _ := adm.do("PUT", "/api/admin/users/admin-1", map[string]any{"id": "someone-else", "display_name": "x", "roles": []string{"admin"}}, &e); code != 400 {
		t.Fatalf("renaming: %d", code)
	}
}

func TestFiveWrongPasswordsOverHTTPLockTheAccount(t *testing.T) {
	a := newAccRig(t, AccountsOptions{LoginBurst: 100})
	a.user("cash-1", "", rbac.Cashier)
	b := a.browser()
	var e errAnswer
	var hdr http.Header
	var code int
	for i := 0; i < 5; i++ {
		code, hdr = b.do("POST", "/api/login", map[string]string{"username": "cash-1", "password": "wrong password number " + string(rune('a'+i))}, &e)
	}
	if code != 429 || e.Code != "locked" || hdr.Get("Retry-After") == "" || e.RetryAfter < 800 || e.RetryAfter > 920 { // about 15 minutes, in seconds
		t.Fatalf("the fifth wrong password: %d %+v retry-after %q", code, e, hdr.Get("Retry-After"))
	}
	if code, _ := b.do("POST", "/api/login", map[string]string{"username": "cash-1", "password": goodPassword}, &e); code != 429 {
		t.Fatalf("the right password while locked: %d", code)
	}
}

func TestSignInAttemptsFromOneAddressAreLimited(t *testing.T) {
	now := time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC)
	a := newAccRig(t, AccountsOptions{LoginBurst: 3, LoginPeriod: time.Minute, Now: func() time.Time { return now }})
	b := a.browser()
	for i := 0; i < 3; i++ {
		if code := b.status("POST", "/api/login", map[string]string{"username": "nobody-" + string(rune('a'+i)), "password": "x"}); code != 401 {
			t.Fatalf("attempt %d: %d", i+1, code)
		}
	}
	var e errAnswer
	code, hdr := b.do("POST", "/api/login", map[string]string{"username": "nobody-d", "password": "x"}, &e)
	if code != 429 || e.Code != "rate_limited" || hdr.Get("Retry-After") == "" || e.RetryAfter < 1 {
		t.Fatalf("the fourth attempt in a minute: %d %+v", code, e)
	}
}

func TestARequestMadeByAnotherSiteIsRefusedEvenWithTheCookie(t *testing.T) {
	a := newAccRig(t, AccountsOptions{})
	a.user("cash-1", "", rbac.Cashier)
	b := a.browser()
	b.signIn("cash-1")
	logout := "/api/logout"
	cases := []struct {
		name, site, origin string
		want               int
	}{
		{"its own page", "same-origin", "", 200},
		{"typed in the address bar", "none", "", 200},
		{"another site", "cross-site", "", 403},
		{"a subdomain, which is not this site", "same-site", "", 403},
		{"an old browser that sends only Origin, from elsewhere", "", "http://evil.example", 403},
		{"an old browser, from here", "", "SELF", 200},
		{"a script outside a browser", "", "", 200},
	}
	for _, c := range cases {
		b.signIn("cash-1")
		b.site, b.origin = c.site, c.origin
		if c.origin == "SELF" {
			b.origin = a.srv.URL
		}
		var e errAnswer
		code, _ := b.do("POST", logout, nil, &e)
		if code != c.want || (c.want == 403 && e.Code != "cross_site") {
			t.Errorf("%s: %d %+v, want %d", c.name, code, e, c.want)
		}
		b.site, b.origin = "same-origin", ""
	}
	// Signing in is protected too, or a page of another site could sign a person in as itself.
	b2 := a.browser()
	b2.site = "cross-site"
	if code := b2.status("POST", "/api/login", map[string]string{"username": "cash-1", "password": goodPassword}); code != 403 {
		t.Fatalf("a sign-in made by another site: %d", code)
	}
}

func TestSigningOutEndsTheSessionAndClearsTheCookie(t *testing.T) {
	a := newAccRig(t, AccountsOptions{})
	a.user("cash-1", "", rbac.Cashier)
	b := a.browser()
	b.signIn("cash-1")
	token := b.cookie("kitchen_session").Value
	code, hdr := b.do("POST", "/api/logout", nil, nil)
	if code != 200 || !strings.Contains(hdr.Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("logout: %d, Set-Cookie %q", code, hdr.Get("Set-Cookie"))
	}
	// The old token is dead even if someone kept a copy of it.
	other := a.browser()
	req := other.request("GET", "/api/orders", nil)
	req.AddCookie(&http.Cookie{Name: "kitchen_session", Value: token})
	resp, err := other.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("a signed-out token still works: %d", resp.StatusCode)
	}
	if code := b.status("POST", "/api/logout", nil); code != 401 {
		t.Fatalf("signing out twice: %d", code)
	}
}

func TestAPersonSeesTheirSessionsAndEndsTheOthers(t *testing.T) {
	a := newAccRig(t, AccountsOptions{})
	a.user("cash-1", "", rbac.Cashier)
	phone, tablet := a.browser(), a.browser()
	phone.signIn("cash-1")
	tablet.signIn("cash-1")
	var list struct{ Sessions []sessionView }
	if code, _ := phone.do("GET", "/api/me/sessions", nil, &list); code != 200 || len(list.Sessions) != 2 {
		t.Fatalf("sessions: %d %+v", code, list)
	}
	current := 0
	for _, s := range list.Sessions {
		if s.Current {
			current++
		}
	}
	if current != 1 {
		t.Fatalf("%d of the sessions say they are the current one", current)
	}
	var out struct{ Revoked int }
	if code, _ := phone.do("POST", "/api/me/sessions/revoke-others", nil, &out); code != 200 || out.Revoked != 1 {
		t.Fatalf("revoke others: %d %+v", code, out)
	}
	if code := tablet.status("GET", "/api/orders", nil); code != 401 {
		t.Fatalf("the other session after it was ended: %d", code)
	}
	if code := phone.status("GET", "/api/orders", nil); code != 200 {
		t.Fatalf("the session that ended the others: %d", code)
	}
	// Ending the session in use by number is refused: that is what signing out is for.
	var mine int64
	phone.do("GET", "/api/me/sessions", nil, &list)
	for _, s := range list.Sessions {
		if s.Current {
			mine = s.ID
		}
	}
	var e errAnswer
	if code, _ := phone.do("POST", "/api/me/sessions/"+strconv.FormatInt(mine, 10)+"/revoke", nil, &e); code != 400 {
		t.Fatalf("ending the session in use: %d %+v", code, e)
	}
	if code, _ := phone.do("POST", "/api/me/sessions/999999/revoke", nil, &e); code != 404 {
		t.Fatalf("ending a session that is not mine: %d", code)
	}
}

func TestTheTrailShowsWhoDidWhatToWhom(t *testing.T) {
	a := newAccRig(t, AccountsOptions{})
	a.user("admin-1", "", rbac.Admin)
	adm := a.browser()
	adm.signIn("admin-1")
	adm.do("POST", "/api/admin/users", map[string]any{"id": "cash-1", "display_name": "Cash One", "roles": []string{"cashier"}}, nil)
	adm.status("POST", "/api/admin/users/cash-1/disable", nil)
	var out struct{ Events []eventView }
	if code, _ := adm.do("GET", "/api/admin/events?subject=cash-1", nil, &out); code != 200 || len(out.Events) < 2 {
		t.Fatalf("events: %d %+v", code, out)
	}
	if e := out.Events[0]; e.Action != "user.disable" || e.Actor != "admin-1" || e.Subject != "cash-1" || e.IP == "" || !json.Valid(e.Detail) {
		t.Fatalf("the newest event is %+v", e)
	}
	if code := adm.status("GET", "/api/admin/events?limit=0", nil); code != 400 {
		t.Fatalf("a bad limit: %d", code)
	}
}

func TestAStreamEndsWhenItsSessionEndsOrItsRolesChange(t *testing.T) {
	old := streamKeepAlive
	streamKeepAlive = 40 * time.Millisecond
	t.Cleanup(func() { streamKeepAlive = old })

	a := newAccRig(t, AccountsOptions{})
	a.user("cash-1", "", rbac.Cashier)
	a.user("cash-2", "", rbac.Cashier)
	a.user("admin-1", "", rbac.Admin)
	adm := a.browser()
	adm.signIn("admin-1")

	open := func(id string) (*bufio.Reader, func()) {
		b := a.browser()
		b.signIn(id)
		resp, err := (&http.Client{Jar: b.jar}).Do(b.request("GET", "/api/stream", nil)) // no timeout: a stream is long
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("stream as %s: %v %v", id, err, resp)
		}
		return bufio.NewReader(resp.Body), func() { resp.Body.Close() }
	}
	// untilReset reads the stream; true when it says "reset" and ends.
	untilReset := func(r *bufio.Reader) bool {
		done := make(chan bool, 1)
		go func() {
			sawReset := false
			for {
				line, err := r.ReadString('\n')
				if strings.HasPrefix(line, "event: reset") {
					sawReset = true
				}
				if err != nil {
					done <- sawReset
					return
				}
			}
		}()
		select {
		case v := <-done:
			return v
		case <-time.After(3 * time.Second):
			return false
		}
	}

	r1, close1 := open("cash-1")
	defer close1()
	r2, close2 := open("cash-2")
	defer close2()
	adm.status("POST", "/api/admin/users/cash-1/disable", nil)
	adm.status("PUT", "/api/admin/users/cash-2", map[string]any{"display_name": "Cash Two", "roles": []string{"manager"}})
	if !untilReset(r1) {
		t.Fatal("the stream of a disabled account did not end")
	}
	if !untilReset(r2) {
		t.Fatal("the stream of an account whose roles changed did not end")
	}
}

func TestAnIdleStreamDoesNotKeepASessionAlive(t *testing.T) {
	// The check that a stream makes at every keep-alive must not count as use, or a screen left
	// open for a week would never go idle.
	old := streamKeepAlive
	streamKeepAlive = 30 * time.Millisecond
	t.Cleanup(func() { streamKeepAlive = old })
	a := newAccRig(t, AccountsOptions{})
	a.user("cash-1", "", rbac.Cashier)
	b := a.browser()
	b.signIn("cash-1")
	resp, err := (&http.Client{Jar: b.jar}).Do(b.request("GET", "/api/stream", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil { // the stream is open
		t.Fatal(err)
	}
	// Make the session look unused for five minutes, now that the stream is open: a check that
	// counted as use would write the time again within a keep-alive or two.
	if _, err := a.pool.Exec(ctxBackground, `UPDATE sessions SET last_seen_at = last_seen_at - interval '5 minutes'`); err != nil {
		t.Fatal(err)
	}
	before := a.lastSeen("cash-1")
	time.Sleep(400 * time.Millisecond) // about ten keep-alives
	if after := a.lastSeen("cash-1"); !after.Equal(before) {
		t.Fatalf("the session was used by the stream: %v then %v", before, after)
	}
}

func (a *accRig) lastSeen(id string) time.Time {
	a.t.Helper()
	var tm time.Time
	if err := a.pool.QueryRow(ctxBackground, `SELECT max(last_seen_at) FROM sessions WHERE user_id = $1`, id).Scan(&tm); err != nil {
		a.t.Fatal(err)
	}
	return tm
}

func TestWhenTheDatabaseIsDownThePeopleAreToldSoNotThatTheirPasswordIsWrong(t *testing.T) {
	a := newAccRig(t, AccountsOptions{})
	a.user("cash-1", "", rbac.Cashier)
	b := a.browser()
	b.signIn("cash-1")
	var h struct{ Status, Accounts string }
	if code, _ := b.do("GET", "/healthz", nil, &h); code != 200 || h.Accounts != "ok" {
		t.Fatalf("healthz: %d %+v", code, h)
	}
	a.pool.Close()
	var e errAnswer
	if code, hdr := b.do("GET", "/api/orders", nil, &e); code != 503 || e.Code != "unavailable" || hdr.Get("Retry-After") == "" {
		t.Fatalf("a request with the database down: %d %+v", code, e)
	}
	if strings.Contains(e.Error, "pool") || strings.Contains(e.Error, "closed") {
		t.Fatalf("the answer leaks the cause: %q", e.Error)
	}
	if code, _ := b.do("POST", "/api/login", map[string]string{"username": "cash-1", "password": goodPassword}, &e); code != 503 {
		t.Fatalf("signing in with the database down: %d", code)
	}
	if code, _ := b.do("GET", "/healthz", nil, &h); code != 503 || h.Accounts != "not answering" {
		t.Fatalf("healthz with the database down: %d %+v", code, h)
	}
	// The menu needs no account, and is still served.
	if code := b.status("GET", "/api/menu", nil); code != 200 {
		t.Fatalf("the menu with the database down: %d", code)
	}
}
