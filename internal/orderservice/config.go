package orderservice

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/menu"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/money"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/promo"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/stock"
)

// The files the service is started with describe the venue: what it sells, its offers, who
// may do what. They are read once at start; none of it is stored in the log, so a change is
// made by editing the file and restarting. An order keeps the prices and names it was taken
// with, so a changed menu does not touch the orders that exist.

// decodeStrict reads one JSON value and refuses unknown fields, so that a typo in a key is
// an error and not a setting that silently does nothing.
func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected data after the end of the document")
	}
	return nil
}

func readFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return data, nil
}

// --- the menu ---------------------------------------------------------------

type optionFile struct {
	ID     string           `json:"id"`
	Name   string           `json:"name"`
	Delta  int64            `json:"delta"`
	Recipe map[string]int64 `json:"recipe"`
}

type groupFile struct {
	ID      string       `json:"id"`
	Name    string       `json:"name"`
	Min     int          `json:"min"`
	Max     int          `json:"max"`
	Options []optionFile `json:"options"`
}

type timePriceFile struct {
	Days  []string `json:"days"` // "mon", "tue", ...; none means every day
	From  string   `json:"from"` // "15:00"
	To    string   `json:"to"`   // "17:00", the end is not included
	Price int64    `json:"price"`
}

type itemFile struct {
	ID         string           `json:"id"`
	Name       string           `json:"name"`
	Category   string           `json:"category"`
	Station    string           `json:"station"`
	Price      int64            `json:"price"`
	TimePrices []timePriceFile  `json:"time_prices"`
	Groups     []groupFile      `json:"groups"`
	Recipe     map[string]int64 `json:"recipe"`
	Active     *bool            `json:"active"` // true if absent
}

type slotFile struct {
	Name  string   `json:"name"`
	Items []string `json:"items"`
}

type comboFile struct {
	ID     string     `json:"id"`
	Name   string     `json:"name"`
	Price  int64      `json:"price"`
	Slots  []slotFile `json:"slots"`
	Active *bool      `json:"active"`
}

// MenuFile is the document that describes the menu. Prices and amounts are whole đồng;
// recipes are whole numbers in the base unit of each ingredient.
type MenuFile struct {
	Items  []itemFile  `json:"items"`
	Combos []comboFile `json:"combos"`
	// LowAt is, for each ingredient, the quantity at or below which it counts as running low.
	LowAt map[string]int64 `json:"low_at"`
}

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// minutes reads "15:30" as minutes since midnight; "24:00" is the end of the day.
func minutes(s string) (int, error) {
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil || h < 0 || h > 24 || m < 0 || m > 59 || (h == 24 && m != 0) {
		return 0, fmt.Errorf("%q is not a time of day like 15:30", s)
	}
	return h*60 + m, nil
}

func recipe(r map[string]int64) menu.Recipe {
	if r == nil {
		return nil
	}
	out := make(menu.Recipe, len(r))
	for k, v := range r {
		out[menu.Ingredient(k)] = v
	}
	return out
}

// LoadMenu reads the menu document and returns the menu and the low-stock thresholds.
func LoadMenu(path string) (*menu.Menu, map[stock.Ingredient]int64, error) {
	data, err := readFile(path)
	if err != nil {
		return nil, nil, err
	}
	return ParseMenu(data)
}

// ParseMenu is LoadMenu for a document already read.
func ParseMenu(data []byte) (*menu.Menu, map[stock.Ingredient]int64, error) {
	var f MenuFile
	if err := decodeStrict(data, &f); err != nil {
		return nil, nil, fmt.Errorf("menu: %w", err)
	}
	var items []menu.Item
	for _, it := range f.Items {
		mi := menu.Item{
			ID: menu.ItemID(it.ID), Name: it.Name, Category: it.Category, Station: menu.Station(it.Station),
			Price: money.Money(it.Price), Recipe: recipe(it.Recipe), Active: it.Active == nil || *it.Active,
		}
		for _, tp := range it.TimePrices {
			from, err := minutes(tp.From)
			if err != nil {
				return nil, nil, fmt.Errorf("menu: item %q: %w", it.ID, err)
			}
			to, err := minutes(tp.To)
			if err != nil {
				return nil, nil, fmt.Errorf("menu: item %q: %w", it.ID, err)
			}
			t := menu.TimePrice{From: from, To: to, Price: money.Money(tp.Price)}
			for _, d := range tp.Days {
				wd, ok := weekdays[strings.ToLower(d)]
				if !ok {
					return nil, nil, fmt.Errorf("menu: item %q: %q is not a day (mon, tue, ...)", it.ID, d)
				}
				t.Days = append(t.Days, wd)
			}
			mi.TimePrices = append(mi.TimePrices, t)
		}
		for _, g := range it.Groups {
			mg := menu.Group{ID: menu.GroupID(g.ID), Name: g.Name, Min: g.Min, Max: g.Max}
			for _, o := range g.Options {
				mg.Options = append(mg.Options, menu.Option{ID: menu.OptionID(o.ID), Name: o.Name, Delta: money.Money(o.Delta), Recipe: recipe(o.Recipe)})
			}
			mi.Groups = append(mi.Groups, mg)
		}
		items = append(items, mi)
	}
	var combos []menu.Combo
	for _, c := range f.Combos {
		mc := menu.Combo{ID: menu.ComboID(c.ID), Name: c.Name, Price: money.Money(c.Price), Active: c.Active == nil || *c.Active}
		for _, s := range c.Slots {
			sl := menu.Slot{Name: s.Name}
			for _, id := range s.Items {
				sl.Items = append(sl.Items, menu.ItemID(id))
			}
			mc.Slots = append(mc.Slots, sl)
		}
		combos = append(combos, mc)
	}
	m, err := menu.New(items, combos)
	if err != nil {
		return nil, nil, fmt.Errorf("menu: %w", err)
	}
	low := map[stock.Ingredient]int64{}
	for ing, at := range f.LowAt {
		low[stock.Ingredient(ing)] = at
	}
	return m, low, nil
}

// --- promotions ---------------------------------------------------------------

type scopeFile struct {
	Categories []string `json:"categories"`
	Items      []string `json:"items"`
}

type promoFile struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Kind        string    `json:"kind"` // percent_off, fixed_off, buy_x_get_y
	Scope       scopeFile `json:"scope"`
	Code        string    `json:"code"`
	Percent     int64     `json:"percent_bp"` // basis points: 1000 is 10%
	Amount      int64     `json:"amount"`
	Buy         int       `json:"buy"`
	Free        int       `json:"free"`
	MinSubtotal int64     `json:"min_subtotal"`
	MaxDiscount int64     `json:"max_discount"`
	ValidFrom   string    `json:"valid_from"` // RFC 3339, optional
	ValidTo     string    `json:"valid_to"`
	Exclusive   bool      `json:"exclusive"`
	Priority    int       `json:"priority"`
	SingleUse   bool      `json:"single_use"`
}

// LoadPromotions reads the promotions document: a JSON list of offers. A missing path
// means no promotions.
func LoadPromotions(path string) (*promo.Set, error) {
	if path == "" {
		return promo.NewSet(nil)
	}
	data, err := readFile(path)
	if err != nil {
		return nil, err
	}
	return ParsePromotions(data)
}

// ParsePromotions is LoadPromotions for a document already read.
func ParsePromotions(data []byte) (*promo.Set, error) {
	var files []promoFile
	if err := decodeStrict(data, &files); err != nil {
		return nil, fmt.Errorf("promotions: %w", err)
	}
	var ps []promo.Promotion
	for _, f := range files {
		p := promo.Promotion{
			ID: promo.ID(f.ID), Name: f.Name, Kind: promo.Kind(f.Kind), Code: f.Code,
			Percent: f.Percent, Amount: money.Money(f.Amount), Buy: f.Buy, Free: f.Free,
			MinSubtotal: money.Money(f.MinSubtotal), MaxDiscount: money.Money(f.MaxDiscount),
			Exclusive: f.Exclusive, Priority: f.Priority, SingleUse: f.SingleUse,
			Scope: promo.Scope{Categories: f.Scope.Categories},
		}
		for _, id := range f.Scope.Items {
			p.Scope.Items = append(p.Scope.Items, menu.ItemID(id))
		}
		var err error
		if p.ValidFrom, err = optionalTime(f.ValidFrom); err != nil {
			return nil, fmt.Errorf("promotions: %q: valid_from: %w", f.ID, err)
		}
		if p.ValidTo, err = optionalTime(f.ValidTo); err != nil {
			return nil, fmt.Errorf("promotions: %q: valid_to: %w", f.ID, err)
		}
		ps = append(ps, p)
	}
	set, err := promo.NewSet(ps)
	if err != nil {
		return nil, fmt.Errorf("promotions: %w", err)
	}
	return set, nil
}

func optionalTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, s)
}

// --- people -------------------------------------------------------------------

type userFile struct {
	ID      string   `json:"id"`
	Token   string   `json:"token"`
	Roles   []string `json:"roles"`
	Station string   `json:"station"`
}

// Users knows who is calling, from a bearer token.
//
// This is the simplest thing that makes the roles real: a list of people and a secret token
// for each, kept in a file that only the service reads. It is not a login system: there are
// no passwords, sessions or expiry, and a token is as good as the file it sits in.
type Users struct {
	list []user
}

type user struct {
	token []byte
	actor rbac.Actor
}

// Shortest token accepted. Tokens are chosen by whoever writes the file; a short one is a
// guess away.
const minTokenLength = 16

// LoadUsers reads the users document: a JSON list of {id, token, roles, station}.
func LoadUsers(path string) (*Users, error) {
	data, err := readFile(path)
	if err != nil {
		return nil, err
	}
	return ParseUsers(data)
}

// ParseUsers is LoadUsers for a document already read.
func ParseUsers(data []byte) (*Users, error) {
	var files []userFile
	if err := decodeStrict(data, &files); err != nil {
		return nil, fmt.Errorf("users: %w", err)
	}
	u := &Users{}
	ids, tokens := map[string]bool{}, map[string]bool{}
	for _, f := range files {
		if len(f.Token) < minTokenLength {
			return nil, fmt.Errorf("users: %q: a token is at least %d characters", f.ID, minTokenLength)
		}
		if ids[f.ID] || tokens[f.Token] {
			return nil, fmt.Errorf("users: %q: an ID or a token is used twice", f.ID)
		}
		ids[f.ID], tokens[f.Token] = true, true
		a := rbac.Actor{ID: f.ID, Station: f.Station}
		for _, r := range f.Roles {
			a.Roles = append(a.Roles, rbac.Role(r))
		}
		if err := a.Validate(); err != nil {
			return nil, fmt.Errorf("users: %w", err)
		}
		u.list = append(u.list, user{token: []byte(f.Token), actor: a})
	}
	if len(u.list) == 0 {
		return nil, errors.New("users: the list is empty, so nobody could use the service")
	}
	return u, nil
}

// Authenticate returns the person behind the request's bearer token. Every token is
// compared, and in constant time, so the time taken does not say how much of a guess was right.
func (u *Users) Authenticate(r *http.Request) (rbac.Actor, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return rbac.Actor{}, false
	}
	given := []byte(strings.TrimSpace(h[len(prefix):]))
	var found rbac.Actor
	ok := false
	for _, c := range u.list {
		if subtle.ConstantTimeCompare(c.token, given) == 1 {
			found, ok = c.actor, true
		}
	}
	return found, ok
}
