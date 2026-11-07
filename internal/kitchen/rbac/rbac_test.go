package rbac

import (
	"os"
	"strings"
	"testing"
)

func holders(p Permission) []Role {
	var out []Role
	for _, r := range AllRoles() {
		if Allowed(r, p) {
			out = append(out, r)
		}
	}
	return out
}

func TestEveryRoleHasWorkAndEveryPermissionHasAnOwner(t *testing.T) {
	for _, r := range AllRoles() {
		if !r.Valid() {
			t.Errorf("role %q is listed but has no entry in the matrix", r)
		}
		if len(Permissions(r)) == 0 {
			t.Errorf("role %q can do nothing", r)
		}
	}
	if len(matrix) != len(AllRoles()) {
		t.Errorf("the matrix has %d roles but AllRoles lists %d", len(matrix), len(AllRoles()))
	}
	for _, p := range AllPermissions() {
		if len(holders(p)) == 0 {
			t.Errorf("permission %q is granted to nobody", p)
		}
	}
	// A permission used in the matrix must be one the code declares.
	known := map[Permission]bool{}
	for _, p := range declared {
		known[p] = true
	}
	for r, ps := range matrix {
		for _, p := range ps {
			if !known[p] {
				t.Errorf("role %q holds %q, which is not declared", r, p)
			}
		}
	}
}

func TestNoRoleHoldsAPermissionTwice(t *testing.T) {
	for r, ps := range matrix {
		seen := map[Permission]bool{}
		for _, p := range ps {
			if seen[p] {
				t.Errorf("role %q lists %q twice", r, p)
			}
			seen[p] = true
		}
	}
}

// Who may do each thing that moves money or decides on it. These are the rules a
// reviewer would ask about first, so they are spelled out one by one.
func TestTheRulesAboutMoneyAndApprovals(t *testing.T) {
	want := map[Permission][]Role{
		RecordPayment: {Cashier},
		ApproveCancel: {Manager},
		IssueRefund:   {Manager},
		ManageUsers:   {Admin},
		StartTicket:   {Kitchen},
		FinishTicket:  {Kitchen},
		ReceiveStock:  {Storekeeper},
		ManageMenu:    {Manager},
		ManagePromos:  {Manager},
		ViewReports:   {Manager},
	}
	for p, roles := range want {
		got := holders(p)
		if len(got) != len(roles) {
			t.Errorf("%s: held by %v, want %v", p, got, roles)
			continue
		}
		for i := range got {
			if got[i] != roles[i] {
				t.Errorf("%s: held by %v, want %v", p, got, roles)
			}
		}
	}
}

func TestSeparationOfDuties(t *testing.T) {
	// The administrator manages accounts and reads the audit log, and cannot touch
	// orders, money, stock or the menu.
	for _, p := range Permissions(Admin) {
		if p != ManageUsers && p != ViewAudit {
			t.Errorf("admin holds %q", p)
		}
	}
	// The cashier takes payments but cannot approve a cancellation or a refund.
	for _, p := range []Permission{ApproveCancel, IssueRefund, ManageMenu, AdjustStock} {
		if Allowed(Cashier, p) {
			t.Errorf("cashier holds %q", p)
		}
	}
	// A manager is trusted with approvals but does not take payments or run accounts.
	for _, p := range []Permission{RecordPayment, ManageUsers, StartTicket, ReceiveStock} {
		if Allowed(Manager, p) {
			t.Errorf("manager holds %q", p)
		}
	}
}

func TestACustomerHoldsNothingThatBelongsToStaff(t *testing.T) {
	allowed := map[Permission]bool{PlaceOrder: true, ViewOwnOrders: true, CancelUnpaid: true, RequestCancel: true}
	for _, p := range Permissions(Customer) {
		if !allowed[p] {
			t.Errorf("a customer can %q", p)
		}
	}
	for _, p := range []Permission{ViewAllOrders, RecordPayment, HandOver, ViewStock, ViewAudit} {
		if Allowed(Customer, p) {
			t.Errorf("a customer holds %q", p)
		}
	}
}

func TestSeveralRolesAddUp(t *testing.T) {
	both := []Role{Cashier, Manager}
	if !Can(both, RecordPayment) || !Can(both, ApproveCancel) {
		t.Fatal("a cashier who is also a manager must hold both sets")
	}
	if Can([]Role{Cashier}, ApproveCancel) {
		t.Fatal("a cashier alone must not approve")
	}
	if Can(nil, PlaceOrder) || Can([]Role{"wizard"}, PlaceOrder) {
		t.Fatal("no role, or an unknown one, holds nothing")
	}
}

func TestActorValidation(t *testing.T) {
	good := Actor{ID: "u1", Roles: []Role{Cashier}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, a := range map[string]Actor{
		"no id":        {Roles: []Role{Cashier}},
		"no role":      {ID: "u"},
		"unknown role": {ID: "u", Roles: []Role{"wizard"}},
		"one bad role": {ID: "u", Roles: []Role{Cashier, "wizard"}},
	} {
		if a.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if !good.Can(PlaceOrder) || good.Can(ApproveCancel) || !good.Has(Cashier) || good.Has(Manager) {
		t.Fatal("Can/Has disagree with the roles")
	}
}

func TestPermissionsAreSortedAndACopy(t *testing.T) {
	ps := Permissions(Manager)
	for i := 1; i < len(ps); i++ {
		if ps[i-1] >= ps[i] {
			t.Fatalf("not sorted: %v", ps)
		}
	}
	ps[0] = "tampered"
	if Allowed(Manager, "tampered") || Permissions(Manager)[0] == "tampered" {
		t.Fatal("Permissions returned the matrix itself")
	}
}

// The table in the documentation is printed from the matrix, and this test keeps
// the two from drifting apart. Regenerate with: UPDATE_DOCS=1 go test ./internal/kitchen/rbac
func TestTheRolesDocumentIsUpToDate(t *testing.T) {
	const path = "../../../docs/kitchen/roles.md"
	want := "# Roles and permissions\n\n" +
		"Generated from `internal/kitchen/rbac` by `UPDATE_DOCS=1 go test ./internal/kitchen/rbac`; do not edit by hand.\n" +
		"The test that compares this file with the code fails when they differ.\n\n" + Markdown()
	if os.Getenv("UPDATE_DOCS") != "" {
		if err := os.MkdirAll("../../../docs/kitchen", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (create it with UPDATE_DOCS=1)", err)
	}
	if strings.ReplaceAll(string(got), "\r\n", "\n") != want {
		t.Fatal("docs/kitchen/roles.md does not match the matrix; regenerate it with UPDATE_DOCS=1")
	}
}

func TestMarkdownListsEveryRoleAndPermission(t *testing.T) {
	md := Markdown()
	for _, r := range AllRoles() {
		if !strings.Contains(md, string(r)) {
			t.Errorf("the table lacks role %s", r)
		}
	}
	for _, p := range AllPermissions() {
		if !strings.Contains(md, "`"+string(p)+"`") {
			t.Errorf("the table lacks permission %s", p)
		}
	}
	if rows := strings.Count(md, "\n"); rows != len(AllPermissions())+2 {
		t.Errorf("%d lines for %d permissions", rows, len(AllPermissions()))
	}
}
