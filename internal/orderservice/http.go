package orderservice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/menu"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/money"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/order"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/stock"
)

const maxBody = 64 << 10

// streamKeepAlive is how often an idle stream sends a comment, so that proxies keep it open.
// It is a variable so that a test can shorten it.
var streamKeepAlive = 15 * time.Second

// API is the HTTP interface of the service: JSON over HTTP, a bearer token for everything but
// the menu and the health check, and a stream of changes for screens that must stay current.
type API struct {
	svc    *Service
	users  *Users
	logger *slog.Logger
}

// NewAPI returns the handler of the service.
func NewAPI(svc *Service, users *Users, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	a := &API{svc: svc, users: users, logger: logger}
	mux := http.NewServeMux()
	registerAssets(mux)
	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("GET /api/menu", a.menu)
	mux.HandleFunc("GET /api/me", a.auth(a.me))

	mux.HandleFunc("POST /api/orders", a.auth(a.placeOrder))
	mux.HandleFunc("GET /api/orders", a.auth(a.listOrders))
	mux.HandleFunc("GET /api/orders/{id}", a.auth(a.getOrder))
	mux.HandleFunc("GET /api/orders/{id}/events", a.auth(a.orderEvents))
	mux.HandleFunc("POST /api/orders/{id}/pay", a.auth(a.pay))
	mux.HandleFunc("POST /api/orders/{id}/tickets/{station}/start", a.auth(a.ticket(true)))
	mux.HandleFunc("POST /api/orders/{id}/tickets/{station}/finish", a.auth(a.ticket(false)))
	mux.HandleFunc("POST /api/orders/{id}/handover", a.auth(a.handOver))
	mux.HandleFunc("POST /api/orders/{id}/cancel", a.auth(a.reasoned(a.svc.CancelUnpaid)))
	mux.HandleFunc("POST /api/orders/{id}/cancel-request", a.auth(a.reasoned(a.svc.RequestCancel)))
	mux.HandleFunc("POST /api/orders/{id}/cancel-reject", a.auth(a.reasoned(a.svc.RejectCancel)))
	mux.HandleFunc("POST /api/orders/{id}/cancel-approve", a.auth(a.approveCancel))
	mux.HandleFunc("POST /api/orders/{id}/refund", a.auth(a.refund))

	mux.HandleFunc("GET /api/stock", a.auth(a.getStock))
	mux.HandleFunc("POST /api/stock/receive", a.auth(a.moveStock(stock.Receive)))
	mux.HandleFunc("POST /api/stock/write-off", a.auth(a.moveStock(stock.WriteOff)))
	mux.HandleFunc("POST /api/stock/count", a.auth(a.moveStock(stock.Count)))

	mux.HandleFunc("GET /api/stream", a.auth(a.stream))
	return secure(a.recoverer(mux))
}

func (a *API) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				a.logger.Error("a request panicked", "path", r.URL.Path, "panic", v)
				writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal error", Code: "internal"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (a *API) auth(f func(http.ResponseWriter, *http.Request, rbac.Actor)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := a.users.Authenticate(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="kitchen"`)
			writeJSON(w, http.StatusUnauthorized, errorBody{Error: "a valid bearer token is needed", Code: "unauthorized"})
			return
		}
		f(w, r, actor)
	}
}

// --- answers --------------------------------------------------------------------

type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
	// Lines says which lines of a cart were refused, and why.
	Lines []lineProblem `json:"lines,omitempty"`
	// Shortages lists the ingredients that were short.
	Shortages []shortage `json:"shortages,omitempty"`
}

type lineProblem struct {
	Line  int    `json:"line"` // from 1
	Error string `json:"error"`
}

type shortage struct {
	Ingredient string `json:"ingredient"`
	Need       int64  `json:"need"`
	Have       int64  `json:"have"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// status maps an error to an HTTP status and a stable code for screens.
func status(err error) (int, string) {
	var cart *CartError
	var oos *OutOfStockError
	var short *stock.ShortageError
	switch {
	case errors.As(err, &cart):
		return http.StatusUnprocessableEntity, "cart"
	case errors.As(err, &oos), errors.As(err, &short):
		return http.StatusConflict, "out_of_stock"
	case errors.Is(err, ErrBadCommandID), errors.Is(err, order.ErrInvalid), errors.Is(err, order.ErrAmount),
		errors.Is(err, order.ErrReasonRequired), errors.Is(err, ErrUnknownIngredient),
		errors.Is(err, stock.ErrQuantity), errors.Is(err, stock.ErrReason), errors.Is(err, stock.ErrNegative):
		return http.StatusBadRequest, "invalid"
	case errors.Is(err, order.ErrNotAllowed), errors.Is(err, order.ErrNotYours), errors.Is(err, order.ErrWrongStation),
		errors.Is(err, order.ErrSelfApproval), errors.Is(err, ErrNotAllowed):
		return http.StatusForbidden, "forbidden"
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, order.ErrWrongState), errors.Is(err, order.ErrCancelPending), errors.Is(err, order.ErrNoCancelRequest),
		errors.Is(err, order.ErrRefundPolicy), errors.Is(err, order.ErrNoSuchTicket), errors.Is(err, ErrConflict):
		return http.StatusConflict, "conflict"
	case errors.Is(err, ErrUnavailable):
		return http.StatusServiceUnavailable, "unavailable"
	}
	return http.StatusInternalServerError, "internal"
}

func (a *API) fail(w http.ResponseWriter, err error) {
	code, kind := status(err)
	body := errorBody{Error: err.Error(), Code: kind}
	var cart *CartError
	var oos *OutOfStockError
	var short *stock.ShortageError
	switch {
	case errors.As(err, &cart):
		for _, l := range cart.Lines {
			body.Lines = append(body.Lines, lineProblem{Line: l.Index + 1, Error: l.Err.Error()})
		}
	case errors.As(err, &oos):
		for _, s := range oos.Shortages {
			body.Shortages = append(body.Shortages, shortage{Ingredient: string(s.Ingredient), Need: s.Need, Have: s.Have})
		}
	case errors.As(err, &short):
		for _, s := range short.Missing {
			body.Shortages = append(body.Shortages, shortage{Ingredient: string(s.Ingredient), Need: s.Need, Have: s.Have})
		}
	}
	switch kind {
	case "internal":
		a.logger.Error("a request failed", "err", err)
		body.Error = "internal error" // the cause is in the log, not in the answer
	case "unavailable":
		w.Header().Set("Retry-After", "1")
	}
	writeJSON(w, code, body)
}

// decode reads the JSON body into v, strictly: unknown fields and trailing data are errors.
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		return fmt.Errorf("%w: reading the request: %v", order.ErrInvalid, err)
	}
	if err := decodeStrict(body, v); err != nil {
		return fmt.Errorf("%w: %v", order.ErrInvalid, err)
	}
	return nil
}

// --- handlers -------------------------------------------------------------------

func (a *API) health(w http.ResponseWriter, _ *http.Request) {
	h := a.svc.Health()
	code := http.StatusOK
	if h.Stale {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{"status": map[bool]string{true: "ok", false: "behind the log"}[!h.Stale], "orders": h.Orders, "next_offset": h.Next, "skipped_records": h.Skipped})
}

func (a *API) menu(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, a.svc.Menu()) }

func (a *API) me(w http.ResponseWriter, _ *http.Request, actor rbac.Actor) {
	roles := make([]string, len(actor.Roles))
	for i, r := range actor.Roles {
		roles[i] = string(r)
	}
	perms := map[string]bool{}
	for _, r := range actor.Roles {
		for _, p := range rbac.Permissions(r) {
			perms[string(p)] = true
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": actor.ID, "roles": roles, "station": actor.Station, "permissions": perms})
}

type pickJSON struct {
	Slot    int                 `json:"slot"`
	Item    string              `json:"item"`
	Options map[string][]string `json:"options"`
}

type selectionJSON struct {
	Item     string              `json:"item"`
	Combo    string              `json:"combo"`
	Options  map[string][]string `json:"options"`
	Picks    []pickJSON          `json:"picks"`
	Quantity int                 `json:"quantity"`
	Note     string              `json:"note"`
}

func choice(m map[string][]string) menu.Choice {
	if m == nil {
		return nil
	}
	c := make(menu.Choice, len(m))
	for g, opts := range m {
		for _, o := range opts {
			c[menu.GroupID(g)] = append(c[menu.GroupID(g)], menu.OptionID(o))
		}
	}
	return c
}

func (s selectionJSON) selection() menu.Selection {
	sel := menu.Selection{Item: menu.ItemID(s.Item), Combo: menu.ComboID(s.Combo), Options: choice(s.Options), Quantity: s.Quantity, Note: s.Note}
	for _, p := range s.Picks {
		sel.Picks = append(sel.Picks, menu.Pick{Slot: p.Slot, Item: menu.ItemID(p.Item), Options: choice(p.Options)})
	}
	return sel
}

type placeBody struct {
	Command  string          `json:"command_id"`
	Items    []selectionJSON `json:"items"`
	Codes    []string        `json:"codes"`
	Customer string          `json:"customer"`
}

func (a *API) placeOrder(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	var b placeBody
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	req := PlaceRequest{Command: b.Command, Codes: b.Codes, Customer: b.Customer}
	for _, it := range b.Items {
		req.Selections = append(req.Selections, it.selection())
	}
	p, err := a.svc.Place(r.Context(), actor, req)
	if err != nil {
		a.fail(w, err)
		return
	}
	if p.Rejected == nil {
		p.Rejected = []string{}
	}
	writeJSON(w, http.StatusCreated, map[string]any{"order": p.Order, "rejected_codes": p.Rejected})
}

func (a *API) listOrders(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	f := OrderFilter{Station: r.URL.Query().Get("station")}
	if s := r.URL.Query().Get("status"); s != "" {
		for _, st := range strings.Split(s, ",") {
			f.Statuses = append(f.Statuses, order.Status(strings.TrimSpace(st)))
		}
	}
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			a.fail(w, fmt.Errorf("%w: limit is a positive number", order.ErrInvalid))
			return
		}
		f.Limit = n
	}
	writeJSON(w, http.StatusOK, map[string]any{"orders": a.svc.Orders(actor, f)})
}

func (a *API) getOrder(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	v, err := a.svc.Order(actor, order.ID(r.PathValue("id")))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) orderEvents(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	evs, err := a.svc.Events(actor, order.ID(r.PathValue("id")))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": evs})
}

// commandBody is what the commands that need nothing else send.
type commandBody struct {
	Command string `json:"command_id"`
}

type reasonBody struct {
	Command string `json:"command_id"`
	Reason  string `json:"reason"`
}

type payBody struct {
	Command   string `json:"command_id"`
	Method    string `json:"method"`
	Tendered  int64  `json:"tendered"`
	Reference string `json:"reference"`
}

func (a *API) pay(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	var b payBody
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	v, err := a.svc.Pay(r.Context(), actor, order.ID(r.PathValue("id")), b.Command,
		order.PayInput{Method: b.Method, Tendered: money.Money(b.Tendered), Reference: b.Reference})
	a.answer(w, v, err)
}

func (a *API) answer(w http.ResponseWriter, v OrderView, err error) {
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) ticket(start bool) func(http.ResponseWriter, *http.Request, rbac.Actor) {
	return func(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
		var b commandBody
		if err := decode(w, r, &b); err != nil {
			a.fail(w, err)
			return
		}
		id, st := order.ID(r.PathValue("id")), menu.Station(r.PathValue("station"))
		var v OrderView
		var err error
		if start {
			v, err = a.svc.StartTicket(r.Context(), actor, id, b.Command, st)
		} else {
			v, err = a.svc.FinishTicket(r.Context(), actor, id, b.Command, st)
		}
		a.answer(w, v, err)
	}
}

func (a *API) handOver(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	var b commandBody
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	v, err := a.svc.HandOver(r.Context(), actor, order.ID(r.PathValue("id")), b.Command)
	a.answer(w, v, err)
}

// reasoned makes the handler of a command that carries a reason.
func (a *API) reasoned(f func(context.Context, rbac.Actor, order.ID, string, string) (OrderView, error)) func(http.ResponseWriter, *http.Request, rbac.Actor) {
	return func(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
		var b reasonBody
		if err := decode(w, r, &b); err != nil {
			a.fail(w, err)
			return
		}
		v, err := f(r.Context(), actor, order.ID(r.PathValue("id")), b.Command, b.Reason)
		a.answer(w, v, err)
	}
}

func (a *API) approveCancel(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	var b struct {
		Command string `json:"command_id"`
		Refund  int64  `json:"refund"`
	}
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	v, err := a.svc.ApproveCancel(r.Context(), actor, order.ID(r.PathValue("id")), b.Command, money.Money(b.Refund))
	a.answer(w, v, err)
}

func (a *API) refund(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	var b struct {
		Command string `json:"command_id"`
		Amount  int64  `json:"amount"`
		Reason  string `json:"reason"`
	}
	if err := decode(w, r, &b); err != nil {
		a.fail(w, err)
		return
	}
	v, err := a.svc.IssueRefund(r.Context(), actor, order.ID(r.PathValue("id")), b.Command, money.Money(b.Amount), b.Reason)
	a.answer(w, v, err)
}

func (a *API) getStock(w http.ResponseWriter, _ *http.Request, actor rbac.Actor) {
	v, err := a.svc.Stock(actor)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) moveStock(kind stock.Kind) func(http.ResponseWriter, *http.Request, rbac.Actor) {
	return func(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
		var b struct {
			Command    string `json:"command_id"`
			Ingredient string `json:"ingredient"`
			Qty        int64  `json:"qty"`
			Reason     string `json:"reason"`
		}
		if err := decode(w, r, &b); err != nil {
			a.fail(w, err)
			return
		}
		v, err := a.svc.MoveStock(r.Context(), actor, b.Command, kind, b.Ingredient, b.Qty, b.Reason)
		if err != nil {
			a.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	}
}

// stream sends the changes the person may see, as server-sent events. A screen reads the
// state first and then listens: an event only says that something changed, and the screen
// asks again for what it shows. If the stream ends with a "reset" event, the screen reads the
// state again and reconnects.
func (a *API) stream(w http.ResponseWriter, r *http.Request, actor rbac.Actor) {
	fl, ok := w.(http.Flusher)
	if !ok {
		a.fail(w, errors.New("streaming is not supported here"))
		return
	}
	sub := a.svc.Subscribe(actor)
	defer sub.Close()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "retry: 2000\n\n")
	fl.Flush()

	tick := time.NewTicker(streamKeepAlive)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			_, _ = io.WriteString(w, ": keep-alive\n\n")
			fl.Flush()
		case ch, open := <-sub.C:
			if !open {
				_, _ = io.WriteString(w, "event: reset\ndata: {}\n\n")
				fl.Flush()
				return
			}
			var buf bytes.Buffer
			if ch.Order != "" {
				writeEvent(&buf, "order", map[string]any{"offset": ch.Offset, "order": ch.Order, "number": ch.Number, "type": ch.Type, "status": ch.Status})
			}
			if ch.StockChanged {
				writeEvent(&buf, "stock", map[string]any{"offset": ch.Offset})
			}
			_, _ = w.Write(buf.Bytes())
			fl.Flush()
		}
	}
}

func writeEvent(buf *bytes.Buffer, name string, v any) {
	data, _ := json.Marshal(v)
	fmt.Fprintf(buf, "id: %d\nevent: %s\ndata: %s\n\n", v.(map[string]any)["offset"], name, data)
}
