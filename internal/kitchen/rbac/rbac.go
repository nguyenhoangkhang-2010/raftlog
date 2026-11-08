// Package rbac says who may do what in the kitchen system.
//
// The rules are data, in one table (see matrix), so they can be read, tested as a
// whole and printed into the documentation. Two principles shape the table:
//
//   - Separation of duties. The person who records a payment is not the one who
//     approves its cancellation, and the administrator who manages accounts cannot
//     touch money or orders at all. Even a manager cannot approve a cancellation
//     that the manager asked for; that rule lives with the order, which knows who
//     asked (see package order).
//   - A role is a set of permissions and nothing else. Whether someone may act on a
//     particular order or a particular station is a business rule of the order, not
//     of this table.
package rbac

import (
	"fmt"
	"sort"
	"strings"
)

// Role is a kind of user.
type Role string

// The roles.
const (
	Customer    Role = "customer"
	Cashier     Role = "cashier"
	Kitchen     Role = "kitchen"
	Runner      Role = "runner"
	Manager     Role = "manager"
	Storekeeper Role = "storekeeper"
	Admin       Role = "admin"
)

// Permission is one thing a role may be allowed to do.
type Permission string

// The permissions.
const (
	PlaceOrder    Permission = "order.place"
	ViewOwnOrders Permission = "order.view_own"
	ViewAllOrders Permission = "order.view_all"
	RecordPayment Permission = "order.record_payment"
	CancelUnpaid  Permission = "order.cancel_unpaid"
	RequestCancel Permission = "order.request_cancel"
	ApproveCancel Permission = "order.approve_cancel"
	IssueRefund   Permission = "order.issue_refund"
	StartTicket   Permission = "ticket.start"
	FinishTicket  Permission = "ticket.finish"
	HandOver      Permission = "order.hand_over"
	ManageMenu    Permission = "menu.manage"
	ManagePromos  Permission = "promo.manage"
	ViewReports   Permission = "report.view"
	ViewStock     Permission = "stock.view"
	ReceiveStock  Permission = "stock.receive"
	AdjustStock   Permission = "stock.adjust"
	ManageUsers   Permission = "user.manage"
	OpenShift     Permission = "shift.open"
	CloseShift    Permission = "shift.close"
	ViewAudit     Permission = "audit.view"
)

// matrix is the whole policy: the permissions of each role.
var matrix = map[Role][]Permission{
	Customer: {PlaceOrder, ViewOwnOrders, CancelUnpaid, RequestCancel},
	Cashier: {
		PlaceOrder, ViewAllOrders, RecordPayment, CancelUnpaid, RequestCancel, HandOver,
		OpenShift, CloseShift,
	},
	Kitchen: {ViewAllOrders, StartTicket, FinishTicket, ViewStock},
	Runner:  {ViewAllOrders, HandOver},
	Manager: {
		ViewAllOrders, CancelUnpaid, RequestCancel, ApproveCancel, IssueRefund,
		ManageMenu, ManagePromos, ViewReports, ViewStock, AdjustStock,
		OpenShift, CloseShift, ViewAudit,
	},
	Storekeeper: {ViewStock, ReceiveStock, AdjustStock},
	Admin:       {ManageUsers, ViewAudit},
}

// AllRoles returns every role, in a fixed order.
func AllRoles() []Role {
	return []Role{Customer, Cashier, Kitchen, Runner, Manager, Storekeeper, Admin}
}

// AllPermissions returns every permission, sorted by name.
func AllPermissions() []Permission {
	seen := map[Permission]bool{}
	for _, ps := range matrix {
		for _, p := range ps {
			seen[p] = true
		}
	}
	// A permission nobody holds still exists; list the ones the code knows about.
	for _, p := range declared {
		seen[p] = true
	}
	out := make([]Permission, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// declared lists every constant above, so that a permission that was added but
// granted to nobody is noticed by the tests.
var declared = []Permission{
	PlaceOrder, ViewOwnOrders, ViewAllOrders, RecordPayment, CancelUnpaid, RequestCancel,
	ApproveCancel, IssueRefund, StartTicket, FinishTicket, HandOver, ManageMenu, ManagePromos,
	ViewReports, ViewStock, ReceiveStock, AdjustStock, ManageUsers, OpenShift, CloseShift, ViewAudit,
}

// Valid reports whether r is one of the roles.
func (r Role) Valid() bool {
	_, ok := matrix[r]
	return ok
}

// Permissions returns what a role may do, sorted by name; nil for an unknown role.
func Permissions(r Role) []Permission {
	ps := append([]Permission(nil), matrix[r]...)
	sort.Slice(ps, func(i, j int) bool { return ps[i] < ps[j] })
	return ps
}

// Allowed reports whether the role holds the permission.
func Allowed(r Role, p Permission) bool {
	for _, q := range matrix[r] {
		if q == p {
			return true
		}
	}
	return false
}

// Can reports whether any of the roles holds the permission. A person may hold
// several roles (a manager who also works the till); an unknown role holds nothing.
func Can(roles []Role, p Permission) bool {
	for _, r := range roles {
		if Allowed(r, p) {
			return true
		}
	}
	return false
}

// Actor is somebody doing something: who, in which roles, and for a kitchen
// worker, at which station.
type Actor struct {
	ID    string
	Roles []Role
	// Station is the kitchen station a kitchen worker is working at, such as
	// "drinks". Empty for everyone else.
	Station string
}

// Can reports whether the actor holds the permission.
func (a Actor) Can(p Permission) bool { return Can(a.Roles, p) }

// Has reports whether the actor holds the role.
func (a Actor) Has(r Role) bool {
	for _, q := range a.Roles {
		if q == r {
			return true
		}
	}
	return false
}

// Validate checks that the actor has an ID and only known roles.
func (a Actor) Validate() error {
	if a.ID == "" {
		return fmt.Errorf("rbac: an actor needs an ID")
	}
	if len(a.Roles) == 0 {
		return fmt.Errorf("rbac: actor %q has no role", a.ID)
	}
	for _, r := range a.Roles {
		if !r.Valid() {
			return fmt.Errorf("rbac: actor %q has the unknown role %q", a.ID, r)
		}
	}
	return nil
}

// Markdown renders the matrix as a table, for the documentation.
func Markdown() string {
	roles := AllRoles()
	var b strings.Builder
	b.WriteString("| Permission |")
	for _, r := range roles {
		fmt.Fprintf(&b, " %s |", r)
	}
	b.WriteString("\n|---|")
	for range roles {
		b.WriteString("---|")
	}
	b.WriteString("\n")
	for _, p := range AllPermissions() {
		fmt.Fprintf(&b, "| `%s` |", p)
		for _, r := range roles {
			if Allowed(r, p) {
				b.WriteString(" ✓ |")
			} else {
				b.WriteString("  |")
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}
