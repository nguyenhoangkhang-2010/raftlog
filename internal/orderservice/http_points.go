package orderservice

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/accounts"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
)

// ErrNoSuchCustomer is returned when the cashier names, for an order at the counter, a login that is not a
// customer's.
var ErrNoSuchCustomer = errors.New("orderservice: there is no customer with that login")

func (a *API) pointsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/points", a.auth(a.getPoints))
}

// PointsAnswer is the points of a customer with the rules of the program, for the screens.
type PointsAnswer struct {
	// Enabled says whether the venue has a points program at all.
	Enabled bool `json:"enabled"`
	// EarnPer is the money paid that earns a point, Value what a point takes off, and MaxShare the percentage of
	// an order that points may pay for.
	EarnPer  int64 `json:"earn_per"`
	Value    int64 `json:"value"`
	MaxShare int64 `json:"max_share"`
	PointsView
}

func (a *API) getPoints(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	customer := actor.ID
	if c := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("customer"))); c != "" && c != actor.ID {
		if !a.svc.seesAll(actor) {
			a.fail(w, fmt.Errorf("%w: another customer's points", ErrNotAllowed))
			return
		}
		customer = c
	} else if !actor.Can(rbac.ViewOwnOrders) && !a.svc.seesAll(actor) {
		a.fail(w, fmt.Errorf("%w: needs %s", ErrNotAllowed, rbac.ViewOwnOrders))
		return
	}
	writeJSON(w, http.StatusOK, a.svc.PointsOf(customer))
}

// customerLogin checks, for an order taken at the counter, that the login the cashier named is a customer's:
// points must not go to a name that nobody owns, nor to a person who registers it later. Without accounts
// (the file of tokens) there is nothing to check against.
func (a *API) customerLogin(ctx context.Context, login string) (string, error) {
	login = strings.ToLower(strings.TrimSpace(login))
	if login == "" || a.acc == nil {
		return login, nil
	}
	u, err := a.acc.GetUser(ctx, login)
	switch {
	case errors.Is(err, accounts.ErrNotFound):
		return "", ErrNoSuchCustomer
	case err != nil:
		return "", err
	case u.Disabled || !hasCustomerRole(u.Roles):
		return "", ErrNoSuchCustomer
	}
	return login, nil
}

func hasCustomerRole(roles []rbac.Role) bool {
	for _, r := range roles {
		if r == rbac.Customer {
			return true
		}
	}
	return false
}
