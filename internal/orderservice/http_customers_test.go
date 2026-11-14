package orderservice

import (
	"net/http/httptest"
	"testing"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/accounts"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/pgmigrate"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/pgtest"
)

// newCustomerRig is a service with accounts, registration open and the points program on.
func newCustomerRig(t *testing.T, open bool, signups int) *accRig {
	t.Helper()
	pool := pgtest.Pool(t)
	if _, err := pgmigrate.Apply(ctxBackground, pool, accounts.Migrations()); err != nil {
		t.Fatal(err)
	}
	acc, err := accounts.New(pool, accounts.Config{Hash: accounts.HashParams{Memory: 8, Iterations: 1, Parallelism: 1}})
	if err != nil {
		t.Fatal(err)
	}
	r := newRigWith(t, program)
	r.stockAll()
	options := []APIOption{WithAccounts(acc, AccountsOptions{SignupBurst: signups})}
	if open {
		options = append(options, WithOpenRegistration())
	}
	srv := httptest.NewServer(NewAPI(r.svc, nil, nil, options...))
	t.Cleanup(srv.Close)
	return &accRig{rig: r, acc: acc, pool: pool, srv: srv}
}

type authAnswer struct {
	Mode         string `json:"mode"`
	Registration bool   `json:"registration"`
}

type meAnswer struct {
	ID          string          `json:"id"`
	DisplayName string          `json:"display_name"`
	Roles       []string        `json:"roles"`
	Permissions map[string]bool `json:"permissions"`
}

func signUp(name, display, password string) map[string]string {
	return map[string]string{"username": name, "display_name": display, "password": password}
}

func TestRegistrationIsClosedUnlessTheVenueOpensIt(t *testing.T) {
	a := newCustomerRig(t, false, 0)
	b := a.browser()
	var auth authAnswer
	if code, _ := b.do("GET", "/api/auth", nil, &auth); code != 200 || auth.Registration || auth.Mode != "password" {
		t.Fatalf("auth: %d %+v", code, auth)
	}
	var body errorBody
	if code, _ := b.do("POST", "/api/register", signUp("newcomer", "New Comer", goodPassword), &body); code != 403 || body.Code != "registration_closed" {
		t.Fatalf("closed: %d %+v", code, body)
	}
	if users, _ := a.acc.ListUsers(ctxBackground); len(users) != 0 {
		t.Fatalf("an account was made: %+v", users)
	}
}

func TestAPersonRegistersIsSignedInAndOrdersLikeAnyCustomer(t *testing.T) {
	a := newCustomerRig(t, true, 0)
	b := a.browser()
	var auth authAnswer
	b.do("GET", "/api/auth", nil, &auth)
	if !auth.Registration {
		t.Fatal("the page is not told that registration is open")
	}
	var me meAnswer
	if code, _ := b.do("POST", "/api/register", signUp("linh", "Linh Trần", goodPassword), &me); code != 201 || me.ID != "linh" || me.DisplayName != "Linh Trần" || len(me.Roles) != 1 || me.Roles[0] != "customer" {
		t.Fatalf("register: %d %+v", code, me)
	}
	if !me.Permissions["order.place"] || me.Permissions["user.manage"] || me.Permissions["order.record_payment"] {
		t.Fatalf("what a new customer may do: %+v", me.Permissions)
	}
	// the session works at once
	var again meAnswer
	if code, _ := b.do("GET", "/api/me", nil, &again); code != 200 || again.ID != "linh" {
		t.Fatalf("me: %d %+v", code, again)
	}
	var placed struct{ Order OrderView }
	if code, _ := b.do("POST", "/api/orders", map[string]any{"command_id": a.cmd(), "items": []map[string]any{{"item": "toastie", "quantity": 1}}}, &placed); code != 201 || placed.Order.Customer != "linh" {
		t.Fatalf("place: %d %+v", code, placed)
	}
	var list struct{ Orders []OrderView }
	if code, _ := b.do("GET", "/api/orders", nil, &list); code != 200 || len(list.Orders) != 1 {
		t.Fatalf("history: %d %+v", code, list)
	}
	// the person can sign out and in with the password they chose
	b.status("POST", "/api/logout", nil)
	b2 := a.browser()
	if code := b2.status("POST", "/api/login", map[string]string{"username": "linh", "password": goodPassword}); code != 200 {
		t.Fatalf("sign in again: %d", code)
	}
}

func TestRegistrationRefusesWhatEveryAccountRefusesAndNeverTakesARoleFromTheRequest(t *testing.T) {
	a := newCustomerRig(t, true, 100)
	a.user("taken", "", rbac.Customer)
	b := a.browser()
	var body errorBody
	for name, c := range map[string]struct {
		body any
		code int
		why  string
	}{
		"a taken login":       {signUp("taken", "X", goodPassword), 409, "conflict"},
		"a weak password":     {signUp("newone", "X", "short"), 400, "weak_password"},
		"a short login":       {signUp("ab", "X", goodPassword), 400, "invalid"},
		"no name":             {signUp("newone", " ", goodPassword), 400, "invalid"},
		"a role asked for":    {map[string]any{"username": "newone", "display_name": "X", "password": goodPassword, "roles": []string{"admin"}}, 400, "invalid"},
		"a station asked for": {map[string]any{"username": "newone", "display_name": "X", "password": goodPassword, "station": "hot"}, 400, "invalid"},
	} {
		if code, _ := b.do("POST", "/api/register", c.body, &body); code != c.code || body.Code != c.why {
			t.Errorf("%s: %d %+v, want %d %s", name, code, body, c.code, c.why)
		}
	}
	if users, _ := a.acc.ListUsers(ctxBackground); len(users) != 1 {
		t.Fatalf("the refused ones made accounts: %d", len(users))
	}
	// a request from another site is refused, like a sign-in
	b.site = "cross-site"
	if code := b.status("POST", "/api/register", signUp("newone", "X", goodPassword)); code != 403 {
		t.Errorf("from another site: %d", code)
	}
}

func TestRegistrationIsLimitedPerAddress(t *testing.T) {
	a := newCustomerRig(t, true, 3)
	for i, name := range []string{"first-one", "second-one", "third-one"} {
		if code := a.browser().status("POST", "/api/register", signUp(name, "X", goodPassword)); code != 201 {
			t.Fatalf("registration %d: %d", i+1, code)
		}
	}
	var body errorBody
	b := a.browser()
	code, h := b.do("POST", "/api/register", signUp("fourth-one", "X", goodPassword), &body)
	if code != 429 || body.Code != "rate_limited" || h.Get("Retry-After") == "" {
		t.Fatalf("the fourth: %d %+v", code, body)
	}
	if _, err := a.acc.GetUser(ctxBackground, "fourth-one"); err == nil {
		t.Fatal("the refused one has an account")
	}
	// signing in is not held back by it
	if code := a.browser().status("POST", "/api/login", map[string]string{"username": "first-one", "password": goodPassword}); code != 200 {
		t.Fatalf("sign in: %d", code)
	}
}

type pointsAnswer struct {
	Enabled   bool  `json:"enabled"`
	EarnPer   int64 `json:"earn_per"`
	Value     int64 `json:"value"`
	MaxShare  int64 `json:"max_share"`
	Balance   int64 `json:"balance"`
	Held      int64 `json:"held"`
	Available int64 `json:"available"`
	History   []struct {
		Kind   string `json:"kind"`
		Points int64  `json:"points"`
	} `json:"history"`
}

func TestACustomerEarnsAndUsesPointsThroughTheAPI(t *testing.T) {
	a := newCustomerRig(t, true, 100)
	a.user("cash-1", "", rbac.Cashier)
	cash := a.browser()
	cash.signIn("cash-1")
	b := a.browser()
	b.do("POST", "/api/register", signUp("linh", "Linh", goodPassword), nil)

	var pts pointsAnswer
	if code, _ := b.do("GET", "/api/points", nil, &pts); code != 200 || !pts.Enabled || pts.EarnPer != 10_000 || pts.Value != 100 || pts.MaxShare != 50 || pts.Balance != 0 {
		t.Fatalf("a new customer: %d %+v", code, pts)
	}
	order := func(points int64) (OrderView, int, errorBody) {
		var placed struct{ Order OrderView }
		var body errorBody
		code, _ := b.do("POST", "/api/orders", map[string]any{"command_id": a.cmd(), "items": []map[string]any{{"item": "toastie", "quantity": 2}}, "points": points}, &placed)
		if code != 201 {
			b.do("POST", "/api/orders", map[string]any{"command_id": a.cmd(), "items": []map[string]any{{"item": "toastie", "quantity": 2}}, "points": points}, &body)
		}
		return placed.Order, code, body
	}
	first, code, _ := order(0)
	if code != 201 {
		t.Fatalf("first order: %d", code)
	}
	if code := cash.status("POST", "/api/orders/"+first.ID+"/pay", map[string]any{"command_id": a.cmd(), "method": "card", "reference": "slip-1"}); code != 200 {
		t.Fatalf("pay: %d", code)
	}
	if code, _ := b.do("GET", "/api/points", nil, &pts); code != 200 || pts.Balance != 9 || pts.Available != 9 || len(pts.History) != 1 || pts.History[0].Kind != "earned" {
		t.Fatalf("after the first order: %d %+v", code, pts)
	}
	// too many points: the answer says how many are allowed
	_, code, body := order(10)
	if code != 409 || body.Code != "points" || body.Max != 9 {
		t.Fatalf("too many: %d %+v", code, body)
	}
	second, code, _ := order(9)
	if code != 201 || second.Total != 89_100 || second.Points == nil || second.Points.Used != 9 {
		t.Fatalf("nine points: %d %+v", code, second)
	}
	if code, _ := b.do("GET", "/api/points", nil, &pts); code != 200 || pts.Held != 9 || pts.Available != 0 || pts.Balance != 9 {
		t.Fatalf("held: %d %+v", code, pts)
	}
}

func TestPointsAreReadByTheirOwnerAndByStaffWhoSeeAllOrdersAndNoOneElse(t *testing.T) {
	a := newCustomerRig(t, true, 100)
	a.user("cash-1", "", rbac.Cashier)
	a.user("cook-1", "hot", rbac.Kitchen)
	a.user("adm-1", "", rbac.Admin)
	a.user("cus-2", "", rbac.Customer)
	linh, other, cash, cook, adm, anon := a.browser(), a.browser(), a.browser(), a.browser(), a.browser(), a.browser()
	linh.do("POST", "/api/register", signUp("linh", "Linh", goodPassword), nil)
	other.signIn("cus-2")
	cash.signIn("cash-1")
	cook.signIn("cook-1")
	adm.signIn("adm-1")
	if code := linh.status("GET", "/api/points", nil); code != 200 {
		t.Errorf("own: %d", code)
	}
	if code := linh.status("GET", "/api/points?customer=linh", nil); code != 200 {
		t.Errorf("own, by name: %d", code)
	}
	if code := other.status("GET", "/api/points?customer=linh", nil); code != 403 {
		t.Errorf("another customer's: %d", code)
	}
	if code := cash.status("GET", "/api/points?customer=linh", nil); code != 200 {
		t.Errorf("a cashier reads a customer's: %d", code)
	}
	if code := cook.status("GET", "/api/points?customer=linh", nil); code != 200 {
		t.Errorf("the kitchen sees all orders and so their points: %d", code)
	}
	if code := adm.status("GET", "/api/points?customer=linh", nil); code != 403 {
		t.Errorf("the administrator does not touch orders: %d", code)
	}
	if code := adm.status("GET", "/api/points", nil); code != 403 {
		t.Errorf("the administrator has no points: %d", code)
	}
	if code := anon.status("GET", "/api/points", nil); code != 401 {
		t.Errorf("without signing in: %d", code)
	}
}

func TestAtTheCounterOnlyARealCustomerCanBeNamed(t *testing.T) {
	a := newCustomerRig(t, true, 100)
	a.user("cash-1", "", rbac.Cashier)
	a.user("mgr-1", "", rbac.Manager)
	a.user("cus-1", "", rbac.Customer)
	a.user("cus-off", "", rbac.Customer)
	if _, err := a.acc.SetDisabled(ctxBackground, "test", "cus-off", true, accounts.Client{}); err != nil {
		t.Fatal(err)
	}
	cash := a.browser()
	cash.signIn("cash-1")
	place := func(customer string) (int, errorBody) {
		var body errorBody
		code, _ := cash.do("POST", "/api/orders", map[string]any{"command_id": a.cmd(), "customer": customer, "items": []map[string]any{{"item": "toastie", "quantity": 1}}}, &body)
		return code, body
	}
	for name, want := range map[string]int{"": 201, "cus-1": 201, " CUS-1 ": 201, "nobody-here": 400, "mgr-1": 400, "cus-off": 400} {
		code, body := place(name)
		if code != want || (want == 400 && body.Code != "no_customer") {
			t.Errorf("customer %q: %d %+v, want %d", name, code, body, want)
		}
	}
	// the order of a named customer is theirs, in lower case
	var placed struct{ Order OrderView }
	cash.do("POST", "/api/orders", map[string]any{"command_id": a.cmd(), "customer": " CUS-1 ", "items": []map[string]any{{"item": "toastie", "quantity": 1}}}, &placed)
	if placed.Order.Customer != "cus-1" {
		t.Fatalf("the customer of the order: %q", placed.Order.Customer)
	}
	// and the cashier cannot spend their points
	var body errorBody
	if code, _ := cash.do("POST", "/api/orders", map[string]any{"command_id": a.cmd(), "customer": "cus-1", "points": 5, "items": []map[string]any{{"item": "toastie", "quantity": 1}}}, &body); code != 409 || body.Code != "no_points" {
		t.Fatalf("points at the counter: %d %+v", code, body)
	}
}
