package orderservice

import (
	"testing"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/accounts"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
)

func TestAManagerChangesPointsThroughTheAPIAndTheCustomerSeesWhyButNotWho(t *testing.T) {
	a := newCustomerRig(t, true, 100)
	a.user("mgr-1", "", rbac.Manager)
	a.user("cash-1", "", rbac.Cashier)
	a.user("cus-1", "", rbac.Customer)
	a.user("cus-off", "", rbac.Customer)
	if _, err := a.acc.SetDisabled(ctxBackground, "test", "cus-off", true, accounts.Client{}); err != nil {
		t.Fatal(err)
	}
	mgr, cash, cus, anon := a.browser(), a.browser(), a.browser(), a.browser()
	mgr.signIn("mgr-1")
	cash.signIn("cash-1")
	cus.signIn("cus-1")
	change := func(customer string, points int, reason string) map[string]any {
		return map[string]any{"command_id": a.cmd(), "customer": customer, "points": points, "reason": reason}
	}

	var ans PointsAnswer
	if code, _ := mgr.do("POST", "/api/points/adjust", change(" CUS-1 ", 40, "welcome gift"), &ans); code != 200 {
		t.Fatalf("a manager gives points: %d", code)
	}
	if ans.Balance != 40 || ans.Adjusted != 40 || len(ans.History) != 1 || ans.History[0].By != "mgr-1" || ans.History[0].Reason != "welcome gift" {
		t.Fatalf("the answer: %+v", ans)
	}

	// nobody else may
	for name, who := range map[string]*browser{"cashier": cash, "customer": cus} {
		if code := who.status("POST", "/api/points/adjust", change("cus-1", 5, "why")); code != 403 {
			t.Errorf("%s: %d", name, code)
		}
	}
	if code := anon.status("POST", "/api/points/adjust", change("cus-1", 5, "why")); code != 401 {
		t.Errorf("without signing in: %d", code)
	}

	// what the manager asks for is checked
	for name, c := range map[string]struct {
		body map[string]any
		code int
		kind string
	}{
		"nobody":       {change("nobody-here", 5, "why"), 400, "no_customer"},
		"staff":        {change("cash-1", 5, "why"), 400, "no_customer"},
		"disabled":     {change("cus-off", 5, "why"), 400, "no_customer"},
		"no reason":    {change("cus-1", 5, " "), 400, "invalid"},
		"zero":         {change("cus-1", 0, "why"), 400, "invalid"},
		"too many":     {change("cus-1", int(MaxAdjust)+1, "why"), 400, "invalid"},
		"bad command":  {map[string]any{"command_id": "x", "customer": "cus-1", "points": 5, "reason": "why"}, 400, "invalid"},
		"extra field":  {map[string]any{"command_id": a.cmd(), "customer": "cus-1", "points": 5, "reason": "why", "balance": 9000}, 400, "invalid"},
		"more than it": {change("cus-1", -41, "why"), 409, "points_short"},
	} {
		var body errorBody
		code, _ := mgr.do("POST", "/api/points/adjust", c.body, &body)
		if code != c.code || body.Code != c.kind {
			t.Errorf("%s: %d %+v, want %d %s", name, code, body, c.code, c.kind)
		}
		if name == "more than it" && body.Max != 40 {
			t.Errorf("the most that can be taken: %d", body.Max)
		}
	}
	// the same command twice is one change
	again := change("cus-1", -10, "correction")
	for i := 0; i < 2; i++ {
		if code, _ := mgr.do("POST", "/api/points/adjust", again, &ans); code != 200 || ans.Balance != 30 {
			t.Fatalf("send %d: %d %+v", i+1, code, ans)
		}
	}

	// the customer reads their points: the reason is there, the name of the manager is not
	var mine PointsAnswer
	if code, _ := cus.do("GET", "/api/points", nil, &mine); code != 200 || mine.Balance != 30 || len(mine.History) != 2 {
		t.Fatalf("the customer's points: %d %+v", code, mine)
	}
	for _, e := range mine.History {
		if e.By != "" || e.Reason == "" || e.Kind != PointsAdjusted {
			t.Errorf("an entry as the customer sees it: %+v", e)
		}
	}
	var theirs PointsAnswer
	if code, _ := mgr.do("GET", "/api/points?customer=cus-1", nil, &theirs); code != 200 || theirs.History[0].By != "mgr-1" {
		t.Fatalf("the manager reads: %d %+v", code, theirs)
	}
}
