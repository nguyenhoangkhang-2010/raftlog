// The schedule screen: who works when, the time clock, the hours that were worked and what an hour
// costs. Everybody who works shifts sees their own day, their shifts and their entries, and clocks in and
// out; a manager plans the week, writes down forgotten work, corrects an entry with a reason and sets the
// hourly rates. What is shown is what the service worked out; this file draws it and sends what is typed.
// Like the other scripts, everything shown is put on the page as text, never as HTML.
(function () {
  "use strict";

  function pure(T) {
    function str(v) { return v === undefined || v === null ? "" : String(v); }
    function pad(n) { return String(n).padStart(2, "0"); }
    function iso(d) { return d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate()); }
    function parts(date) { return date.split("-").map(Number); }
    function addDays(date, n) {
      var p = parts(date);
      var d = new Date(Date.UTC(p[0], p[1] - 1, p[2] + n));
      return d.getUTCFullYear() + "-" + pad(d.getUTCMonth() + 1) + "-" + pad(d.getUTCDate());
    }
    function isDay(s) { return /^\d{4}-\d{2}-\d{2}$/.test(str(s)); }
    function isTime(s) { return /^([01]\d|2[0-3]):[0-5]\d$/.test(str(s)); }

    // weekday is 0 for Monday to 6 for Sunday.
    function weekday(date) {
      var p = parts(date);
      return (new Date(Date.UTC(p[0], p[1] - 1, p[2])).getUTCDay() + 6) % 7;
    }
    // weekStart is the Monday of the week that a day is in.
    function weekStart(date) { return addDays(date, -weekday(date)); }
    function weekDays(start) {
      var out = [];
      for (var i = 0; i < 7; i++) out.push(addDays(start, i));
      return out;
    }

    // dayLabel writes a day as its weekday and its day and month, like "Mon 17/11".
    function dayLabel(date) {
      var names = [T("Mon"), T("Tue"), T("Wed"), T("Thu"), T("Fri"), T("Sat"), T("Sun")];
      var p = parts(date);
      return names[weekday(date)] + " " + pad(p[2]) + "/" + pad(p[1]);
    }
    function shortDay(date) { var p = parts(date); return pad(p[2]) + "/" + pad(p[1]); }

    // minutesText writes a number of minutes as hours and minutes.
    function minutesText(m) {
      m = Math.max(0, Math.round(m || 0));
      var hh = Math.floor(m / 60), mm = m % 60;
      if (!hh) return T("{m} min", { m: mm });
      return mm ? T("{h} h {m} min", { h: hh, m: mm }) : T("{h} h", { h: hh });
    }
    // signedMinutes is the same with a sign, for a difference.
    function signedMinutes(m) {
      if (!m) return "0";
      return (m > 0 ? "+" : "−") + minutesText(Math.abs(m));
    }

    // shiftText writes a shift as its times, and marks one that ends on the next day.
    function shiftText(s) {
      return s.start + "–" + s.end + (s.end <= s.start ? " " + T("(next day)") : "");
    }

    // shiftProblem checks a shift as typed, before it is sent. It says what the service would say.
    function shiftProblem(f) {
      if (!str(f.user).trim()) return T("Choose who works the shift.");
      if (!isDay(f.day)) return T("Choose the day.");
      if (!isTime(f.start) || !isTime(f.end)) return T("Give the start and the end, like 08:00.");
      var a = parseInt(f.start.slice(0, 2), 10) * 60 + parseInt(f.start.slice(3), 10);
      var b = parseInt(f.end.slice(0, 2), 10) * 60 + parseInt(f.end.slice(3), 10);
      if (b <= a) b += 24 * 60;
      if (b - a < 30 || b - a > 16 * 60) return T("A shift is from 30 minutes to 16 hours long.");
      if (Array.from(str(f.station).trim()).length > 20) return T("The station is at most 20 characters.");
      return "";
    }

    // entryProblem checks a stretch of work that a manager writes down or corrects. An end is needed unless
    // the stretch is still open.
    function entryProblem(f, mayBeOpen) {
      if (f.user !== undefined && !str(f.user).trim()) return T("Choose who worked.");
      if (!isDay(f.day)) return T("Choose the day.");
      if (!isTime(f.in) || !(isTime(f.out) || (mayBeOpen && str(f.out) === ""))) return T("Give the start and the end, like 08:00.");
      if (!str(f.reason).trim()) return T("Say why.");
      return "";
    }

    // rateProblem checks an hourly rate as typed.
    function rateProblem(f, max) {
      if (!str(f.user).trim()) return T("Choose who the rate is for.");
      if (!isDay(f.from)) return T("Choose the day it starts.");
      var n = /^\d+$/.test(str(f.rate).trim()) ? parseInt(str(f.rate).trim(), 10) : -1;
      if (n < 0 || n > max) return T("The rate is a whole number of đồng for an hour, up to {max}.", { max: max });
      return "";
    }

    // nextShift is the first shift that has not ended at a time (in milliseconds); null if none is left.
    function nextShift(shifts, nowMs) {
      var best = null;
      (shifts || []).forEach(function (s) {
        if (Date.parse(s.ends_at) <= nowMs) return;
        if (!best || Date.parse(s.starts_at) < Date.parse(best.starts_at)) best = s;
      });
      return best;
    }

    // elapsed is how many whole minutes it is from a time to now, both in milliseconds.
    function elapsed(fromMs, nowMs) { return Math.max(0, Math.floor((nowMs - fromMs) / 60000)); }

    // hoursTotals adds up the rows of the hours report.
    function hoursTotals(rows) {
      var t = { planned_minutes: 0, worked_minutes: 0, late: 0, late_minutes: 0, missed: 0, days: 0 };
      (rows || []).forEach(function (r) { Object.keys(t).forEach(function (k) { t[k] += r[k] || 0; }); });
      return t;
    }

    // currentRates picks, for each person, the rate in force on a day: the latest that starts on or before
    // it, the one written last if two start on the same day. rates are as the service lists them, newest
    // first. Each person's earlier rates are kept, newest first, for the history.
    function ratesOf(rates, today) {
      var by = {}, order = [];
      (rates || []).forEach(function (r) {
        if (!by[r.user]) { by[r.user] = { user: r.user, current: null, later: [], earlier: [] }; order.push(r.user); }
        var e = by[r.user];
        if (r.from > today) e.later.push(r);
        else if (!e.current) e.current = r;
        else e.earlier.push(r);
      });
      return order.map(function (u) { return by[u]; });
    }

    // clockState says where a person stands with the clock: "in", "out", or "drawer" for one who is clocked in
    // but has a cash drawer open, who may not clock out yet.
    function clockState(me) {
      if (!me) return "unknown";
      if (me.open) return me.drawer_open ? "drawer" : "in";
      return "out";
    }

    return { str: str, iso: iso, addDays: addDays, isDay: isDay, isTime: isTime, weekday: weekday, weekStart: weekStart, weekDays: weekDays,
      dayLabel: dayLabel, shortDay: shortDay, minutesText: minutesText, signedMinutes: signedMinutes, shiftText: shiftText,
      shiftProblem: shiftProblem, entryProblem: entryProblem, rateProblem: rateProblem, nextShift: nextShift, elapsed: elapsed,
      hoursTotals: hoursTotals, ratesOf: ratesOf, clockState: clockState };
  }

  var Staff = function (ui) {
    var h = ui.h, clear = ui.clear, T = ui.T, api = ui.api, run = ui.run, toast = ui.toast, fieldOf = ui.fieldOf;
    var twoStep = ui.twoStep, when = ui.when, money = ui.money;
    var p = pure(T);

    var R = { tab: null, today: "", skew: 0, me: null, week: "", rota: null, sheet: null, hours: null, rates: null, problem: "",
      form: null, cancelling: null, cancelReason: "", editing: null, history: {}, shownHistory: null,
      from: "", to: "", entry: { user: "", day: "", in: "", out: "", reason: "" }, rate: { user: "", from: "", rate: "" },
      edit: { day: "", in: "", out: "", reason: "" }, tick: null, redraw: function () { /* set by the screen */ } };

    function can(x) { return !!ui.S.me.permissions[x]; }
    function manager() { return can("staff.manage"); }
    function errText(e) { return e && e.message ? e.message : T("The request did not get an answer. Press again to retry."); }
    function fail(e) { toast(errText(e), "bad"); }
    function todayOf() { return R.today || p.iso(new Date()); }
    function nowMs() { return Date.now() + R.skew; }

    // ---- loading -----------------------------------------------------------------------

    function loadMe() {
      if (!can("clock.use")) return Promise.resolve();
      return api("GET", "/api/staff/me").then(function (d) {
        R.me = d; R.today = d.day; R.skew = Date.parse(d.now) - Date.now();
        if (!R.week) R.week = p.weekStart(d.day);
        R.redraw();
      }, function (e) { R.problem = errText(e); R.redraw(); });
    }
    function weekQuery() {
      var q = "from=" + R.week + "&to=" + p.addDays(R.week, 6);
      return manager() ? q + "&cancelled=1" : q;
    }
    function loadRota() {
      if (!R.week) R.week = p.weekStart(todayOf());
      R.rota = undefined; R.redraw();
      return api("GET", "/api/rota?" + weekQuery()).then(function (d) { R.rota = d; R.problem = ""; R.redraw(); },
        function (e) { R.problem = errText(e); R.rota = null; R.redraw(); });
    }
    function periodQuery() { return "from=" + encodeURIComponent(R.from) + "&to=" + encodeURIComponent(R.to); }
    function loadSheet() {
      R.sheet = undefined; R.redraw();
      return api("GET", "/api/timesheet?" + periodQuery()).then(function (d) { R.sheet = d; R.problem = ""; R.redraw(); },
        function (e) { R.problem = errText(e); R.sheet = null; R.redraw(); });
    }
    function loadHours() {
      R.hours = undefined; R.redraw();
      return api("GET", "/api/staff/hours?" + periodQuery()).then(function (d) { R.hours = d; R.problem = ""; R.redraw(); },
        function (e) { R.problem = errText(e); R.hours = null; R.redraw(); });
    }
    function loadRates() {
      R.rates = undefined; R.redraw();
      return api("GET", "/api/staff/rates?history=1").then(function (d) { R.rates = d; R.problem = ""; R.redraw(); },
        function (e) { R.problem = errText(e); R.rates = null; R.redraw(); });
    }
    function loadTab() {
      if (R.tab === "me") return loadMe();
      if (R.tab === "week") return loadRota();
      if (R.tab === "sheet") return loadSheet();
      if (R.tab === "hours") return loadHours();
      if (R.tab === "rates") return loadRates();
    }

    // ---- small parts ---------------------------------------------------------------------

    function table(head, rows, numeric) {
      return h("div", { class: "scroll" }, h("table", { class: "stackrows" },
        h("thead", {}, h("tr", {}, head.map(function (c, i) { return h("th", { class: numeric[i] ? "n" : "" }, c); }))),
        h("tbody", {}, rows.map(function (r) {
          return h("tr", {}, r.map(function (c, i) { return h("td", { class: numeric[i] ? "n" : "", "data-label": i === 0 ? null : head[i] }, c); }));
        }))));
    }

    function input(value, attrs, onchange) {
      var el = h("input", Object.assign({ type: "text", autocomplete: "off" }, attrs));
      el.value = value;
      el.addEventListener("input", function () { onchange(el.value); });
      return el;
    }

    function personSelect(people, value, onchange, label) {
      var sel = h("select", { "aria-label": label }, [h("option", { value: "" }, T("Choose…"))].concat((people || []).map(function (x) {
        return h("option", { value: x.id, selected: x.id === value || null }, x.name || x.id);
      })));
      sel.addEventListener("change", function () { onchange(sel.value); });
      return sel;
    }

    function entryBadges(e) {
      var out = [];
      if (!e.clock_out) out.push(h("span", { class: "badge ready" }, T("still in")));
      if (e.manual) out.push(h("span", { class: "badge placed" }, T("written by a manager")));
      if (e.corrected) out.push(h("span", { class: "badge low" }, T("corrected")));
      return out;
    }
    function entrySpan(e) { return e.in + "–" + (e.clock_out ? (e.out_day ? shortDay(e.out_day) + " " : "") + e.out : "…"); }
    function shortDay(d) { return p.shortDay(d); }

    // ---- my day --------------------------------------------------------------------------

    function clockCard() {
      var me = R.me, st = p.clockState(me), open = me.open;
      var live = h("span", { class: "fine" });
      function tick() { if (open) live.textContent = T("So far: {d}", { d: p.minutesText(p.elapsed(Date.parse(open.clock_in), nowMs())) }); }
      R.tick = tick; tick();
      var box = h("div", { class: "card stack clock" + (open ? " on" : "") });
      box.appendChild(h("div", { class: "row" }, h("h2", { class: "grow" }, T("The time clock")), h("span", { class: "badge " + (open ? "ready" : "placed") }, open ? T("in") : T("out"))));
      box.appendChild(open ? h("p", { class: "clockline" }, T("You clocked in at {time}.", { time: open.in }), " ", live) : h("p", { class: "clockline" }, T("You are not clocked in.")));
      var next = p.nextShift(me.shifts, nowMs());
      if (next) box.appendChild(h("p", { class: "fine" }, T("Next shift: {when}", { when: (next.day === todayOf() ? T("today") : p.dayLabel(next.day)) + " " + p.shiftText(next) + (next.station ? " · " + next.station : "") })));
      if (st === "drawer") box.appendChild(h("div", { class: "banner warn", role: "status" }, T("Your cash drawer is still open. Close the shift before you clock out.")));
      if (st === "out" && me.required && can("shift.open")) box.appendChild(h("div", { class: "banner warn", role: "status" }, T("A till can be opened only by someone who has clocked in.")));
      var btn;
      if (open) {
        btn = twoStep(h("button", { type: "button", class: "primary big", disabled: st === "drawer" || null }, T("Clock out")), T("Clock out"), function (b) {
          run("clock-out", b, function () { return api("POST", "/api/clock/out"); }).then(function (v) { if (v) { toast(T("Clocked out."), "ok"); loadMe(); } });
        });
      } else {
        btn = h("button", { type: "button", class: "primary big" }, T("Clock in"));
        btn.addEventListener("click", function () {
          run("clock-in", btn, function () { return api("POST", "/api/clock/in"); }).then(function (v) { if (v) { toast(T("Clocked in."), "ok"); loadMe(); } });
        });
      }
      box.appendChild(h("div", { class: "row" }, btn));
      return box;
    }

    function myShifts() {
      var card = h("div", { class: "card stack" }, h("h2", {}, T("Your next shifts")));
      var list = (R.me.shifts || []).filter(function (s) { return Date.parse(s.ends_at) > nowMs(); });
      if (!list.length) { card.appendChild(h("p", { class: "empty" }, T("Nothing is planned for you in the next week."))); return card; }
      card.appendChild(h("ul", { class: "rowlist" }, list.map(function (s) {
        return h("li", { class: "rowitem" },
          h("div", { class: "grow" }, h("b", {}, s.day === todayOf() ? T("Today") : p.dayLabel(s.day)), " ", p.shiftText(s), s.note ? h("div", { class: "fine" }, s.note) : null),
          s.station ? h("span", { class: "badge placed" }, s.station) : null);
      })));
      return card;
    }

    function myEntries() {
      var card = h("div", { class: "card stack" }, h("h2", {}, T("Your last two weeks")));
      var list = R.me.entries || [];
      if (!list.length) { card.appendChild(h("p", { class: "empty" }, T("You have not clocked in yet."))); return card; }
      var total = list.reduce(function (s, e) { return s + (e.clock_out ? e.minutes : 0); }, 0);
      card.appendChild(h("ul", { class: "rowlist" }, list.map(function (e) {
        return h("li", { class: "rowitem" },
          h("div", { class: "grow" }, h("b", {}, p.dayLabel(e.day)), " ", entrySpan(e), e.note ? h("div", { class: "fine" }, e.note) : null),
          entryBadges(e), h("b", { class: "money" }, e.clock_out ? p.minutesText(e.minutes) : ""));
      })));
      card.appendChild(h("div", { class: "row" }, h("span", { class: "fine grow" }, T("Total worked")), h("b", { class: "money" }, p.minutesText(total))));
      return card;
    }

    function meView() {
      if (!R.me) return h("div", { class: "card" }, h("p", { class: "empty" }, T("Loading…")));
      return h("div", { class: "stack" }, clockCard(), myShifts(), myEntries());
    }

    // ---- the week ------------------------------------------------------------------------

    function formCard(day) {
      var f = R.form, people = R.rota.people || [];
      var problem = h("p", { class: "problem", role: "alert" });
      var box = h("div", { class: "sub stack" });
      var edit = f.mode === "edit";
      if (!edit) box.appendChild(personSelect(people, f.user, function (v) { f.user = v; }, T("Who works the shift")));
      box.appendChild(h("div", { class: "erow c2" },
        fieldOf("sf-start", T("From"), Object.assign(input(f.start, { type: "time" }, function (v) { f.start = v; }), { id: "sf-start" })),
        fieldOf("sf-end", T("Until"), Object.assign(input(f.end, { type: "time" }, function (v) { f.end = v; }), { id: "sf-end" }))));
      box.appendChild(h("div", { class: "erow c2" },
        fieldOf("sf-station", T("Station"), Object.assign(input(f.station, { maxlength: "20", placeholder: T("such as line, bar, till") }, function (v) { f.station = v; }), { id: "sf-station" })),
        fieldOf("sf-note", T("Note"), Object.assign(input(f.note, { maxlength: "200" }, function (v) { f.note = v; }), { id: "sf-note" }))));
      var save = h("button", { type: "button", class: "small" }, edit ? T("Save the shift") : T("Plan the shift"));
      save.addEventListener("click", function () {
        var why = p.shiftProblem(f);
        if (why) { problem.textContent = why; return; }
        problem.textContent = "";
        var body = { day: day, start: f.start, end: f.end, station: f.station.trim(), note: f.note.trim() };
        if (!edit) body.user = f.user;
        run("shift:" + JSON.stringify(body) + f.id, save, function () { return edit ? api("PUT", "/api/rota/" + f.id, body) : api("POST", "/api/rota", body); })
          .then(function (v) { if (v) { toast(edit ? T("Saved.") : T("Shift planned."), "ok"); R.form = null; loadRota(); } });
      });
      box.appendChild(problem);
      box.appendChild(h("div", { class: "row" }, save, h("button", { type: "button", class: "small ghost", onclick: function () { R.form = null; R.redraw(); } }, T("Never mind"))));
      return box;
    }

    function shiftRow(s, names) {
      var cancelled = s.status === "cancelled";
      var main = h("div", { class: "grow" }, h("b", {}, p.shiftText(s)), names ? " " + (s.name || s.user) : "",
        s.station ? h("small", { class: "fine" }, " · " + s.station) : null, s.note ? h("div", { class: "fine" }, s.note) : null,
        cancelled ? h("div", { class: "fine" }, T("Cancelled by {who}", { who: s.cancelled_by }) + (s.cancel_reason ? ": " + s.cancel_reason : "")) : null);
      var actions = [];
      if (cancelled) actions.push(h("span", { class: "badge cancelled" }, T("Cancelled")));
      else if (manager()) {
        actions.push(h("button", { type: "button", class: "small ghost", onclick: function () {
          R.form = { mode: "edit", id: s.id, day: s.day, start: s.start, end: s.end, station: s.station || "", note: s.note || "" }; R.cancelling = null; R.redraw();
        } }, T("Change")));
        actions.push(h("button", { type: "button", class: "small ghost", onclick: function () { R.cancelling = R.cancelling === s.id ? null : s.id; R.cancelReason = ""; R.form = null; R.redraw(); } }, T("Cancel this shift")));
      }
      var row = h("li", { class: "rowitem" + (cancelled ? " off" : "") }, main, actions);
      if (R.cancelling !== s.id || cancelled) return row;
      var why = input(R.cancelReason, { maxlength: "200", "aria-label": T("Reason"), placeholder: T("Why is it cancelled?") }, function (v) { R.cancelReason = v; });
      var go = twoStep(h("button", { type: "button", class: "small" }, T("Cancel the shift")), T("Cancel the shift"), function (b) {
        run("shift-cancel:" + s.id, b, function () { return api("POST", "/api/rota/" + s.id + "/cancel", { reason: R.cancelReason.trim() }); })
          .then(function (v) { if (v) { toast(T("Shift cancelled."), "ok"); R.cancelling = null; loadRota(); } });
      });
      return [row, h("li", {}, h("div", { class: "sub erow c2" }, why, go))];
    }

    function dayCard(day) {
      var shifts = (R.rota.shifts || []).filter(function (s) { return s.day === day; });
      var isToday = day === todayOf();
      var card = h("div", { class: "card stack daycard" + (isToday ? " today" : "") });
      var head = [h("h2", { class: "grow" }, p.dayLabel(day))];
      if (isToday) head.push(h("span", { class: "badge ready" }, T("today")));
      if (manager()) head.push(h("button", { type: "button", class: "small ghost", onclick: function () {
        R.form = { mode: "add", day: day, user: "", start: "08:00", end: "16:00", station: "", note: "" }; R.cancelling = null; R.redraw();
      } }, T("Add a shift")));
      card.appendChild(h("div", { class: "row" }, head));
      if (!shifts.length) card.appendChild(h("p", { class: "fine" }, manager() ? T("Nobody is planned.") : T("Nothing planned for you.")));
      else card.appendChild(h("ul", { class: "rowlist" }, shifts.map(function (s) { return shiftRow(s, manager()); })));
      if (R.form && R.form.day === day) card.appendChild(formCard(day));
      return card;
    }

    function weekView() {
      var d = R.rota;
      if (d === undefined) return h("div", { class: "card" }, h("p", { class: "empty" }, T("Loading…")));
      if (!d) return null;
      var box = h("div", { class: "stack" });
      var last = p.addDays(R.week, 6);
      var thisWeek = p.weekStart(todayOf());
      var copy = manager() ? twoStep(h("button", { type: "button", class: "small ghost" }, T("Copy the week to the next")), T("Copy the week to the next"), function (b) {
        run("copy-week:" + R.week, b, function () { return api("POST", "/api/rota/copy", { from: R.week, to: last, days: 7 }); }).then(function (v) {
          if (!v) return;
          toast(T("{made} shifts copied; {skipped} left out because they overlap another.", { made: v.made, skipped: v.skipped }), "ok");
          R.week = p.addDays(R.week, 7); loadRota();
        });
      }) : null;
      box.appendChild(h("div", { class: "card stack" },
        h("div", { class: "row" },
          h("button", { type: "button", class: "small ghost", "aria-label": T("The week before"), onclick: function () { R.week = p.addDays(R.week, -7); R.form = null; loadRota(); } }, "‹"),
          h("b", { class: "grow center" }, p.shortDay(R.week) + " – " + p.shortDay(last)),
          h("button", { type: "button", class: "small ghost", "aria-label": T("The week after"), onclick: function () { R.week = p.addDays(R.week, 7); R.form = null; loadRota(); } }, "›")),
        h("div", { class: "row" }, h("button", { type: "button", class: "small " + (R.week === thisWeek ? "" : "ghost"), onclick: function () { R.week = thisWeek; R.form = null; loadRota(); } }, T("This week")), h("span", { class: "grow" }), copy)));
      p.weekDays(R.week).forEach(function (day) { box.appendChild(dayCard(day)); });
      return box;
    }

    // ---- the time sheet ------------------------------------------------------------------

    function periodBar(onShow) {
      var from = h("input", { type: "date", "aria-label": T("From") }), to = h("input", { type: "date", "aria-label": T("Until") });
      from.value = R.from; to.value = R.to;
      from.addEventListener("change", function () { R.from = from.value; });
      to.addEventListener("change", function () { R.to = to.value; });
      var today = todayOf();
      var presets = [[T("This week"), p.weekStart(today), today], [T("Last week"), p.addDays(p.weekStart(today), -7), p.addDays(p.weekStart(today), -1)],
        [T("Last 30 days"), p.addDays(today, -29), today], [T("This month"), today.slice(0, 8) + "01", today]];
      return h("div", { class: "card stack" },
        h("div", { class: "opts" }, presets.map(function (x) {
          return h("button", { type: "button", class: "small " + (R.from === x[1] && R.to === x[2] ? "" : "ghost"), onclick: function () { R.from = x[1]; R.to = x[2]; onShow(); } }, x[0]);
        })),
        h("div", { class: "row" }, h("label", {}, T("From")), from, h("label", {}, T("Until")), to, h("button", { type: "button", class: "small", onclick: onShow }, T("Show"))));
    }

    function onDuty(d) {
      var card = h("div", { class: "card stack" }, h("h2", {}, T("Clocked in now")));
      var list = d.on_duty || [];
      if (!list.length) { card.appendChild(h("p", { class: "empty" }, T("Nobody is clocked in."))); return card; }
      card.appendChild(h("ul", { class: "rowlist" }, list.map(function (e) {
        return h("li", { class: "rowitem" }, h("div", { class: "grow" }, h("b", {}, e.name || e.user), " ", T("since {time}", { time: e.in }), e.day !== todayOf() ? " (" + p.shortDay(e.day) + ")" : ""),
          h("b", { class: "money" }, p.minutesText(p.elapsed(Date.parse(e.clock_in), nowMs()))));
      })));
      return card;
    }

    function entryForm(d) {
      var f = R.entry, problem = h("p", { class: "problem", role: "alert" });
      if (!f.day) f.day = todayOf();
      var btn = h("button", { type: "button", class: "primary" }, T("Write it down"));
      btn.addEventListener("click", function () {
        var why = p.entryProblem(f, false);
        if (why) { problem.textContent = why; return; }
        problem.textContent = "";
        run("entry-add:" + JSON.stringify(f), btn, function () {
          return api("POST", "/api/timesheet", { user: f.user, day: f.day, in: f.in, out: f.out, reason: f.reason.trim() });
        }).then(function (v) { if (v) { toast(T("Written down."), "ok"); R.entry = { user: f.user, day: f.day, in: "", out: "", reason: "" }; loadSheet(); } });
      });
      return h("div", { class: "card stack" }, h("h2", {}, T("Write down work that was not clocked")),
        h("p", { class: "fine" }, T("For someone who forgot to clock in or out. A reason is needed, and the entry is marked as written by a manager.")),
        personSelect(d.people, f.user, function (v) { f.user = v; }, T("Who worked")),
        h("div", { class: "erow c3" }, fieldOf("en-day", T("Day"), Object.assign(input(f.day, { type: "date" }, function (v) { f.day = v; }), { id: "en-day" })),
          fieldOf("en-in", T("From"), Object.assign(input(f.in, { type: "time" }, function (v) { f.in = v; }), { id: "en-in" })),
          fieldOf("en-out", T("Until"), Object.assign(input(f.out, { type: "time" }, function (v) { f.out = v; }), { id: "en-out" }))),
        fieldOf("en-why", T("Reason"), Object.assign(input(f.reason, { maxlength: "200" }, function (v) { f.reason = v; }), { id: "en-why" })),
        problem, h("div", { class: "row" }, btn));
    }

    function loadHistory(id) {
      return api("GET", "/api/timesheet/" + id + "/history").then(function (d) { R.history[id] = d.changes; R.redraw(); }, fail);
    }

    function historyBox(e) {
      var list = R.history[e.id];
      if (list === undefined) return h("p", { class: "fine" }, T("Loading…"));
      if (!list.length) return h("p", { class: "fine" }, T("This entry was never changed."));
      return h("ul", { class: "rowlist" }, list.map(function (c) {
        return h("li", { class: "rowitem" }, h("div", { class: "grow" }, c.old + " → " + c.new, h("div", { class: "fine" }, c.by + " · " + when(c.at) + " · " + c.reason)));
      }));
    }

    function editForm(e) {
      var f = R.edit, problem = h("p", { class: "problem", role: "alert" });
      var save = h("button", { type: "button", class: "small" }, T("Save the correction"));
      save.addEventListener("click", function () {
        var why = p.entryProblem(f, !e.clock_out);
        if (why) { problem.textContent = why; return; }
        problem.textContent = "";
        run("entry-fix:" + e.id + JSON.stringify(f), save, function () { return api("PUT", "/api/timesheet/" + e.id, { day: f.day, in: f.in, out: f.out, reason: f.reason.trim() }); })
          .then(function (v) { if (v) { toast(T("Saved."), "ok"); R.editing = null; delete R.history[e.id]; loadSheet(); } });
      });
      return h("div", { class: "sub stack" },
        h("div", { class: "erow c3" }, fieldOf("ed-day", T("Day"), Object.assign(input(f.day, { type: "date" }, function (v) { f.day = v; }), { id: "ed-day" })),
          fieldOf("ed-in", T("From"), Object.assign(input(f.in, { type: "time" }, function (v) { f.in = v; }), { id: "ed-in" })),
          fieldOf("ed-out", T("Until"), Object.assign(input(f.out, { type: "time" }, function (v) { f.out = v; }), { id: "ed-out" }))),
        fieldOf("ed-why", T("Reason"), Object.assign(input(f.reason, { maxlength: "200", placeholder: T("Why is it changed?") }, function (v) { f.reason = v; }), { id: "ed-why" })),
        problem, h("div", { class: "row" }, save, h("button", { type: "button", class: "small ghost", onclick: function () { R.editing = null; R.redraw(); } }, T("Never mind"))));
    }

    function entriesCard(d) {
      var card = h("div", { class: "card stack" }, h("h2", {}, T("Entries")));
      var list = d.entries || [];
      if (!list.length) { card.appendChild(h("p", { class: "empty" }, T("No one clocked in during this period."))); return card; }
      card.appendChild(h("ul", { class: "rowlist" }, list.map(function (e) {
        var row = h("li", { class: "rowitem" },
          h("div", { class: "grow" }, h("b", {}, e.name || e.user), " ", p.dayLabel(e.day), " ", entrySpan(e), e.note ? h("div", { class: "fine" }, e.note) : null),
          entryBadges(e), h("b", { class: "money" }, e.clock_out ? p.minutesText(e.minutes) : ""),
          h("button", { type: "button", class: "small ghost", onclick: function () {
            R.editing = R.editing === e.id ? null : e.id;
            R.edit = { day: e.day, in: e.in, out: e.out || "", reason: "" }; R.redraw();
          } }, T("Correct")),
          e.corrected ? h("button", { type: "button", class: "small ghost", onclick: function () {
            R.shownHistory = R.shownHistory === e.id ? null : e.id; if (R.shownHistory && R.history[e.id] === undefined) loadHistory(e.id); else R.redraw();
          } }, R.shownHistory === e.id ? T("Hide") : T("History")) : null);
        var more = [row];
        if (R.editing === e.id) more.push(h("li", {}, editForm(e)));
        if (R.shownHistory === e.id && e.corrected) more.push(h("li", {}, h("div", { class: "sub" }, historyBox(e))));
        return more;
      })));
      return card;
    }

    function sheetView() {
      var d = R.sheet;
      if (d === undefined) return h("div", { class: "card" }, h("p", { class: "empty" }, T("Loading…")));
      if (!d) return null;
      return h("div", { class: "stack" }, onDuty(d), entryForm(d), periodBar(loadSheet), entriesCard(d));
    }

    // ---- hours ---------------------------------------------------------------------------

    function hoursView() {
      var d = R.hours;
      var box = h("div", { class: "stack" }, periodBar(loadHours));
      if (d === undefined) { box.appendChild(h("div", { class: "card" }, h("p", { class: "empty" }, T("Loading…")))); return box; }
      if (!d) return box;
      var rows = d.hours || [], t = p.hoursTotals(rows);
      if (!rows.length) { box.appendChild(h("div", { class: "card" }, h("p", { class: "empty" }, T("No shift was planned and no one clocked in during this period.")))); return box; }
      var data = rows.map(function (r) {
        return [manager() ? (r.name || r.user) : T("You"), p.minutesText(r.planned_minutes), p.minutesText(r.worked_minutes), p.signedMinutes(r.worked_minutes - r.planned_minutes),
          String(r.days), r.late ? T("{n} ({m} min)", { n: r.late, m: r.late_minutes }) : "0", String(r.missed)];
      });
      if (manager() && rows.length > 1) data.push([T("Total"), p.minutesText(t.planned_minutes), p.minutesText(t.worked_minutes), p.signedMinutes(t.worked_minutes - t.planned_minutes), "", String(t.late), String(t.missed)]);
      box.appendChild(h("div", { class: "card stack" }, h("h2", {}, T("Planned and worked")),
        table([T("Person"), T("Planned"), T("Worked"), T("Difference"), T("Days"), T("Late"), T("Missed")], data, [false, true, true, true, true, true, true]),
        h("p", { class: "fine" }, T("Planned is the length of the shifts that start in the period; worked is the time clocked, whether or not a shift was planned. Late is the first clock-in of a shift more than five minutes after it starts; missed is a shift that is over with no one clocked in."))));
      return box;
    }

    // ---- rates -----------------------------------------------------------------------------

    function ratesView() {
      var d = R.rates;
      if (d === undefined) return h("div", { class: "card" }, h("p", { class: "empty" }, T("Loading…")));
      if (!d) return null;
      var f = R.rate, problem = h("p", { class: "problem", role: "alert" });
      if (!f.from) f.from = todayOf();
      var btn = h("button", { type: "button", class: "primary" }, T("Set the rate"));
      btn.addEventListener("click", function () {
        var why = p.rateProblem(f, d.max_rate);
        if (why) { problem.textContent = why; return; }
        problem.textContent = "";
        run("rate:" + JSON.stringify(f), btn, function () { return api("POST", "/api/staff/rates", { user: f.user, from: f.from, rate: parseInt(f.rate.trim(), 10) }); })
          .then(function (v) { if (v) { toast(T("Rate set."), "ok"); R.rate = { user: f.user, from: "", rate: "" }; loadRates(); } });
      });
      var box = h("div", { class: "stack" });
      box.appendChild(h("div", { class: "card stack" }, h("h2", {}, T("Set an hourly rate")),
        h("p", { class: "fine" }, T("An hour of work costs this much from the day given on. Earlier rates are kept: the profit report uses the rate in force on the day the work was done.")),
        personSelect(d.people, f.user, function (v) { f.user = v; }, T("Who the rate is for")),
        h("div", { class: "erow c2" }, fieldOf("rt-from", T("From the day"), Object.assign(input(f.from, { type: "date" }, function (v) { f.from = v; }), { id: "rt-from" })),
          fieldOf("rt-rate", T("Đồng an hour"), Object.assign(input(f.rate, { inputmode: "numeric" }, function (v) { f.rate = v; }), { id: "rt-rate" }))),
        problem, h("div", { class: "row" }, btn)));
      var names = {};
      (d.people || []).forEach(function (x) { names[x.id] = x.name; });
      var by = p.ratesOf(d.rates, todayOf());
      var have = {};
      by.forEach(function (e) { have[e.user] = true; });
      var list = h("div", { class: "card stack" }, h("h2", {}, T("Rates")));
      var rows = by.map(function (e) {
        var hist = e.later.concat(e.earlier);
        return h("li", { class: "rowitem" }, h("div", { class: "grow" }, h("b", {}, names[e.user] || e.user),
          e.current ? h("small", { class: "fine" }, " " + T("from {day}", { day: e.current.from })) : null,
          hist.length ? h("div", { class: "fine" }, hist.map(function (r) { return T("from {day}", { day: r.from }) + ": " + money(r.rate); }).join(" · ")) : null),
          e.current ? h("b", { class: "money" }, T("{amount} an hour", { amount: money(e.current.rate) })) : h("span", { class: "badge low" }, T("no rate yet")));
      });
      (d.people || []).forEach(function (x) {
        if (!have[x.id]) rows.push(h("li", { class: "rowitem" }, h("div", { class: "grow" }, h("b", {}, x.name || x.id)), h("span", { class: "badge low" }, T("no rate yet"))));
      });
      list.appendChild(rows.length ? h("ul", { class: "rowlist" }, rows) : h("p", { class: "empty" }, T("Nobody works shifts yet.")));
      list.appendChild(h("p", { class: "fine" }, T("Work done by someone with no rate costs nothing in the profit report, and the report says how many minutes that was.")));
      box.appendChild(list);
      return box;
    }

    // ---- the screen --------------------------------------------------------------------

    function tabs() {
      var out = [];
      if (can("clock.use")) out.push(["me", T("My day")]);
      out.push(["week", T("Rota")]);
      if (manager()) out.push(["sheet", T("Time sheet")]);
      out.push(["hours", T("Hours")]);
      if (manager()) out.push(["rates", T("Pay rates")]);
      return out;
    }

    function screen(root) {
      var nav = h("div", { class: "subnav", role: "tablist" }), body = h("div", { class: "stack" });
      if (!R.from) { var t = p.iso(new Date()); R.from = p.weekStart(t); R.to = t; }
      if (!R.tab || !tabs().some(function (x) { return x[0] === R.tab; })) R.tab = tabs()[0][0];
      function draw() {
        clear(nav); clear(body);
        tabs().forEach(function (x) {
          nav.appendChild(h("button", { type: "button", role: "tab", "aria-selected": R.tab === x[0] ? "true" : "false", onclick: function () { R.tab = x[0]; R.problem = ""; loadTab(); } }, x[1]));
        });
        R.tick = null;
        if (R.problem) body.appendChild(h("div", { class: "banner bad", role: "alert" }, R.problem));
        var view = R.tab === "me" ? meView() : R.tab === "week" ? (R.rota === undefined || R.rota ? weekView() : null) : R.tab === "sheet" ? sheetView() : R.tab === "hours" ? hoursView() : ratesView();
        if (view) body.appendChild(view);
      }
      R.redraw = draw;
      root.appendChild(h("h1", {}, T("Schedule")));
      root.appendChild(h("div", { class: "stack" }, nav, body));
      draw();
      // the day is the venue's, which the service tells; the other tabs need it, so the first look asks for it
      loadMe().then(function () { return R.tab === "me" ? null : loadTab(); });
      return {
        // the elapsed time moves with the clock, not with an event
        update: function () { if (R.tick) R.tick(); },
        reload: function () { if (R.tab === "me" && !R.problem) loadMe(); },
      };
    }

    return { schedule: screen };
  };

  Staff.pure = pure;
  if (typeof module === "object" && module.exports) module.exports = Staff;
  else window.KitchenStaff = Staff;
})();
