// The shift screen: a cashier opens a shift by counting the drawer, records money put in and taken
// out, and closes it by counting again, without being told what the drawer should hold; a manager
// sees every shift, what each drawer should have held and by how much it was off, and reviews it.
// What is shown is what the service worked out; this file draws it and sends what is typed. Like the
// other scripts, everything shown is put on the page as text, never as HTML.
(function () {
  "use strict";

  function pure(T) {
    function str(v) { return v === undefined || v === null ? "" : String(v); }
    function intOf(s) { s = str(s).trim(); return /^-?\d+$/.test(s) ? parseInt(s, 10) : null; }

    // countsOf reads a count as typed (a text for each denomination) and returns the counts the
    // service takes, with the denominations that have none left out, and what they add up to.
    function countsOf(draft) {
      var counts = {}, total = 0;
      var keys = Object.keys(draft || {});
      for (var i = 0; i < keys.length; i++) {
        var raw = str(draft[keys[i]]).trim();
        if (raw === "") continue;
        var n = intOf(raw);
        if (n === null || n < 0 || n > 100000) return { err: T("A count is a whole number of notes or coins, zero or more.") };
        if (n > 0) { counts[keys[i]] = n; total += n * parseInt(keys[i], 10); }
      }
      return { counts: counts, total: total };
    }

    // totalOf adds up a count that came from the service.
    function totalOf(counts) {
      var t = 0;
      Object.keys(counts || {}).forEach(function (d) { t += parseInt(d, 10) * counts[d]; });
      return t;
    }

    // verdict says in a word how a drawer came out: the variance is the count less what it should
    // have held, so it is negative when the drawer is short.
    function verdict(variance) {
      if (variance === 0) return { key: "exact", label: T("The drawer is exact.") };
      if (variance < 0) return { key: "short", label: T("The drawer is short.") };
      return { key: "over", label: T("The drawer is over.") };
    }

    // breakdown lists what the drawer should have held and what it was made of, in order, as
    // [label, amount, sign] where the sign is +1, -1 or 0 for the line that is the result.
    function breakdown(sh) {
      if (sh.expected === undefined || sh.expected === null) return [];
      return [
        [T("Opening count"), sh.opening_total, 1],
        [T("Cash sales"), sh.sales, 1],
        [T("Cash refunds"), sh.refunds, -1],
        [T("Money put in"), sh.pay_in, 1],
        [T("Money taken out"), sh.pay_out, -1],
        [T("Should hold"), sh.expected, 0],
      ];
    }

    // needsLook says that a closed shift is off and has not been looked at.
    function needsLook(sh) {
      return sh.status === "closed" && !sh.reviewed_by && sh.variance !== 0 && sh.variance !== undefined && sh.variance !== null;
    }

    return { intOf: intOf, countsOf: countsOf, totalOf: totalOf, verdict: verdict, breakdown: breakdown, needsLook: needsLook };
  }

  var Shifts = function (ui) {
    var h = ui.h, clear = ui.clear, T = ui.T, api = ui.api, run = ui.run, toast = ui.toast, fieldOf = ui.fieldOf;
    var twoStep = ui.twoStep, when = ui.when, money = ui.money;
    var p = pure(T);

    var W = { current: undefined, denominations: [], required: false, list: null, detail: {}, open: null, openDraft: {}, closeDraft: {}, till: "", note: "",
      movement: { kind: "out", amount: "", reason: "" }, redraw: function () { /* set by the screen */ } };

    function can(x) { return !!ui.S.me.permissions[x]; }
    function errText(e) { return e && e.message ? e.message : T("The request did not get an answer. Press again to retry."); }
    function fail(e) { toast(errText(e), "bad"); }

    function load() {
      var jobs = [api("GET", "/api/shifts/current"), api("GET", "/api/shifts?limit=60")];
      return Promise.all(jobs).then(function (rs) {
        W.current = rs[0].shift; W.denominations = rs[0].denominations; W.required = rs[0].required; W.list = rs[1].shifts; W.redraw();
      }, fail);
    }
    function loadDetail(id) {
      return api("GET", "/api/shifts/" + id).then(function (d) { W.detail[id] = d; W.redraw(); }, fail);
    }

    // ---- a count of the drawer ---------------------------------------------------------

    // countGrid is the notes and coins with a box for each. It writes what is typed into the draft and
    // shows the total as it grows.
    function countGrid(draft, label) {
      var total = h("b", { class: "money" });
      function sum() {
        var r = p.countsOf(draft);
        total.textContent = r.err ? "–" : money(r.total);
      }
      var rows = W.denominations.map(function (d) {
        var input = h("input", { type: "text", inputmode: "numeric", autocomplete: "off", "aria-label": T("{denomination} pieces", { denomination: money(d) }), placeholder: "0" });
        input.value = draft[d] === undefined ? "" : String(draft[d]);
        var line = h("span", { class: "money fine" });
        function show() {
          var n = p.intOf(input.value);
          line.textContent = n && n > 0 ? "= " + money(n * d) : "";
        }
        input.addEventListener("input", function () { draft[d] = input.value; show(); sum(); });
        show();
        return h("div", { class: "countrow" }, h("label", {}, money(d)), input, line);
      });
      sum();
      return h("div", { class: "stack" }, h("div", { class: "countgrid" }, rows), h("div", { class: "row" }, h("span", { class: "fine" }, label), h("span", { class: "grow" }), total));
    }

    // ---- my shift ----------------------------------------------------------------------

    function openCard() {
      var till = h("input", { type: "text", maxlength: "32", autocomplete: "off", autocapitalize: "none", "aria-label": T("Drawer") });
      till.value = W.till || "main";
      till.addEventListener("input", function () { W.till = till.value; });
      var problem = h("p", { class: "problem", role: "alert" });
      var btn = h("button", { type: "button", class: "primary" }, T("Open the shift"));
      btn.addEventListener("click", function () {
        var r = p.countsOf(W.openDraft);
        if (r.err) { problem.textContent = r.err; return; }
        if (!/^[a-z0-9][a-z0-9_-]{0,31}$/.test(till.value.trim())) { problem.textContent = T("Name the drawer with lower-case letters, digits and dashes, like main."); return; }
        problem.textContent = "";
        run("shift-open:" + till.value + ":" + JSON.stringify(r.counts), btn, function () {
          return api("POST", "/api/shifts", { till: till.value.trim(), counts: r.counts });
        }).then(function (v) { if (v) { toast(T("Shift opened."), "ok"); W.openDraft = {}; load(); } });
      });
      return h("div", { class: "card stack" }, h("h2", {}, T("Open a shift")),
        W.required ? h("div", { class: "banner warn", role: "status" }, T("Cash cannot be taken until a shift is open.")) : null,
        h("p", { class: "fine" }, T("Count the drawer before you start: how many notes and coins of each kind.")),
        fieldOf("sh-till", T("Drawer"), Object.assign(till, { id: "sh-till" })),
        countGrid(W.openDraft, T("The drawer holds")), problem, h("div", { class: "row" }, btn));
    }

    function movementForm(sh) {
      var m = W.movement;
      var kind = h("select", { "aria-label": T("In or out") }, [["out", T("Money taken out")], ["in", T("Money put in")]].map(function (k) {
        return h("option", { value: k[0], selected: m.kind === k[0] || null }, k[1]);
      }));
      kind.addEventListener("change", function () { m.kind = kind.value; });
      var amount = h("input", { type: "text", inputmode: "numeric", autocomplete: "off", "aria-label": T("Amount (đồng)"), placeholder: T("Amount (đồng)") });
      amount.value = m.amount; amount.addEventListener("input", function () { m.amount = amount.value; });
      var reason = h("input", { type: "text", maxlength: "200", autocomplete: "off", "aria-label": T("Reason"), placeholder: T("Reason, such as ice or a bank drop") });
      reason.value = m.reason; reason.addEventListener("input", function () { m.reason = reason.value; });
      var problem = h("p", { class: "problem", role: "alert" });
      var btn = h("button", { type: "button", class: "small" }, T("Record"));
      btn.addEventListener("click", function () {
        var n = p.intOf(m.amount);
        if (n === null || n < 1) { problem.textContent = T("The amount is a whole number of đồng above zero."); return; }
        if (!m.reason.trim()) { problem.textContent = T("Say why."); return; }
        problem.textContent = "";
        run("shift-move:" + sh.id + ":" + m.kind + ":" + n + ":" + m.reason, btn, function () {
          return api("POST", "/api/shifts/" + sh.id + "/movements", { kind: m.kind, amount: n, reason: m.reason.trim() });
        }).then(function (v) { if (v) { toast(T("Recorded."), "ok"); W.movement = { kind: m.kind, amount: "", reason: "" }; load(); } });
      });
      return h("div", { class: "stack" }, h("div", { class: "erow c3" }, kind, amount, h("div", {}, btn)), reason, problem);
    }

    function currentCard() {
      var sh = W.current;
      var box = h("div", { class: "stack" });
      box.appendChild(h("div", { class: "card stack" },
        h("div", { class: "row" }, h("h2", { class: "grow" }, T("Your shift")), h("span", { class: "badge ready" }, T("open"))),
        h("dl", { class: "kv" }, h("dt", {}, T("Drawer")), h("dd", {}, sh.till), h("dt", {}, T("Opened")), h("dd", {}, when(sh.opened_at)),
          h("dt", {}, T("Opening count")), h("dd", {}, money(sh.opening_total)))));
      var moves = sh.movements || [];
      var mv = h("div", { class: "card stack" }, h("h2", {}, T("Money in and out")),
        moves.length ? h("ul", { class: "rowlist" }, moves.map(function (x) {
          return h("li", { class: "rowitem" }, h("div", { class: "grow" }, x.reason, h("div", { class: "fine" }, when(x.at))),
            h("div", { class: "money " + (x.kind === "in" ? "ok-text" : "") }, (x.kind === "in" ? "+" : "−") + money(x.amount)));
        })) : h("p", { class: "fine" }, T("Nothing recorded yet.")),
        movementForm(sh));
      box.appendChild(mv);
      var note = h("input", { type: "text", maxlength: "300", autocomplete: "off", "aria-label": T("Note"), placeholder: T("A note, if the count is not what you expect") });
      note.value = W.note; note.addEventListener("input", function () { W.note = note.value; });
      var problem = h("p", { class: "problem", role: "alert" });
      var closeBtn = twoStep(h("button", { type: "button", class: "primary" }, T("Close the shift")), T("Close the shift"), function (btn) {
        var r = p.countsOf(W.closeDraft);
        if (r.err) { problem.textContent = r.err; return; }
        problem.textContent = "";
        run("shift-close:" + sh.id + ":" + JSON.stringify(r.counts), btn, function () {
          return api("POST", "/api/shifts/" + sh.id + "/close", { counts: r.counts, note: W.note.trim() });
        }).then(function (v) {
          if (!v) return;
          W.closeDraft = {}; W.note = ""; W.detail[sh.id] = v; W.result = v.shift.id; load();
        });
      });
      box.appendChild(h("div", { class: "card stack" }, h("h2", {}, T("Close the shift")),
        h("p", { class: "fine" }, T("Count the drawer now. You are not shown what it should hold until the shift is closed.")),
        countGrid(W.closeDraft, T("The drawer holds")), note, problem, h("div", { class: "row" }, closeBtn)));
      return box;
    }

    // ---- a shift, as a list and in detail ------------------------------------------------

    function varianceBadge(sh) {
      if (sh.status === "open") return h("span", { class: "badge placed" }, T("open"));
      var v = p.verdict(sh.variance);
      var text = v.key === "exact" ? T("exact") : (sh.variance > 0 ? "+" : "−") + money(Math.abs(sh.variance));
      return h("span", { class: "badge " + (v.key === "exact" ? "ready" : v.key === "short" ? "cancelled" : "low") }, text);
    }

    function detailCard(sh, d) {
      var box = h("div", { class: "sub stack" });
      if (sh.status === "closed") {
        var v = p.verdict(sh.variance);
        box.appendChild(h("div", { class: "banner " + (v.key === "exact" ? "ok" : v.key === "short" ? "bad" : "warn"), role: "status" },
          v.label + (v.key === "exact" ? "" : " " + money(Math.abs(sh.variance)))));
        box.appendChild(h("table", {}, h("tbody", {}, p.breakdown(sh).map(function (row) {
          var last = row[2] === 0;
          return h("tr", {}, h("td", {}, last ? h("b", {}, row[0]) : row[0]), h("td", { class: "n money" }, last ? h("b", {}, money(row[1])) : (row[2] > 0 ? "+" : "−") + money(row[1])));
        }).concat([h("tr", {}, h("td", {}, h("b", {}, T("Counted in the drawer"))), h("td", { class: "n money" }, h("b", {}, money(sh.closing_total))))]))));
        if (sh.close_note) box.appendChild(h("p", {}, h("span", { class: "fine" }, T("Note") + ": "), sh.close_note));
      } else if (d && d.expected_so_far !== undefined && d.expected_so_far !== null) {
        box.appendChild(h("div", { class: "banner warn", role: "status" }, T("Should hold now: {amount}", { amount: money(d.expected_so_far) })));
      }
      if (d && d.cash && d.cash.entries.length) {
        box.appendChild(h("h2", {}, T("Cash from orders")));
        box.appendChild(h("ul", { class: "rowlist" }, d.cash.entries.map(function (e) {
          return h("li", { class: "rowitem" }, h("div", { class: "grow" }, T("Order {n}", { n: "#" + e.number }), h("div", { class: "fine" }, when(e.at))),
            h("div", { class: "money " + (e.kind === "sale" ? "ok-text" : "") }, (e.kind === "sale" ? "+" : "−") + money(e.amount)));
        })));
      }
      var moves = sh.movements || [];
      if (moves.length) {
        box.appendChild(h("h2", {}, T("Money in and out")));
        box.appendChild(h("ul", { class: "rowlist" }, moves.map(function (x) {
          return h("li", { class: "rowitem" }, h("div", { class: "grow" }, x.reason, h("div", { class: "fine" }, when(x.at) + " · " + x.by)), h("div", { class: "money" }, (x.kind === "in" ? "+" : "−") + money(x.amount)));
        })));
      }
      if (sh.reviewed_by) box.appendChild(h("p", { class: "fine" }, T("Reviewed by {who} {when}", { who: sh.reviewed_by, when: when(sh.reviewed_at) }) + (sh.review_note ? ": " + sh.review_note : "")));
      if (can("shift.review") && sh.status === "closed" && !sh.reviewed_by) {
        var note = h("input", { type: "text", maxlength: "300", autocomplete: "off", "aria-label": T("Note"), placeholder: T("A note on what you found") });
        var btn = h("button", { type: "button", class: "small" }, T("Mark as reviewed"));
        btn.addEventListener("click", function () {
          run("shift-review:" + sh.id + ":" + note.value, btn, function () { return api("POST", "/api/shifts/" + sh.id + "/review", { note: note.value.trim() }); })
            .then(function (r) { if (r) { toast(T("Reviewed."), "ok"); loadDetail(sh.id); load(); } });
        });
        box.appendChild(h("div", { class: "erow c2" }, note, btn));
      }
      if (can("shift.review") && sh.status === "open" && sh.cashier !== ui.S.me.id) {
        var drafts = W.forced || (W.forced = {});
        var draft = drafts[sh.id] || (drafts[sh.id] = {});
        var problem = h("p", { class: "problem", role: "alert" });
        var force = twoStep(h("button", { type: "button", class: "small ghost" }, T("Close it for them")), T("Close it for them"), function (b) {
          var r = p.countsOf(draft);
          if (r.err) { problem.textContent = r.err; return; }
          b.disabled = true;
          api("POST", "/api/shifts/" + sh.id + "/close", { counts: r.counts, note: T("Closed by the manager.") }).then(function (v) {
            toast(T("Shift closed."), "ok"); W.detail[sh.id] = v; load();
          }, fail).then(function () { b.disabled = false; });
        });
        box.appendChild(h("details", {}, h("summary", { class: "fine" }, T("The cashier left without closing: count the drawer and close the shift")), countGrid(draft, T("The drawer holds")), problem, h("div", { class: "row" }, force)));
      }
      return box;
    }

    function listCard() {
      var card = h("div", { class: "card stack" }, h("h2", {}, can("shift.review") ? T("All shifts") : T("Your earlier shifts")));
      var list = (W.list || []).filter(function (s) { return !(W.current && s.id === W.current.id && !can("shift.review")); });
      if (!list.length) { card.appendChild(h("p", { class: "empty" }, T("No shift yet."))); return card; }
      card.appendChild(h("ul", { class: "rowlist" }, list.map(function (s) {
        var open = W.open === s.id;
        var row = h("li", { class: "rowitem" + (open ? " on" : "") },
          h("div", { class: "grow" }, h("b", {}, T("Shift {n}", { n: s.id })), h("small", { class: "fine" }, " " + s.cashier + " · " + s.till),
            h("div", { class: "fine" }, when(s.opened_at) + (s.closed_at ? " → " + when(s.closed_at) : ""))),
          p.needsLook(s) && can("shift.review") ? h("span", { class: "badge low" }, T("to review")) : null,
          varianceBadge(s),
          h("button", { type: "button", class: "small ghost", onclick: function () { W.open = open ? null : s.id; if (!open) loadDetail(s.id); else W.redraw(); } }, open ? T("Hide") : T("Details")));
        return open ? [row, h("li", {}, W.detail[s.id] ? detailCard(W.detail[s.id].shift, W.detail[s.id]) : h("p", { class: "fine" }, T("Loading…")))] : row;
      })));
      return card;
    }

    // ---- the screen --------------------------------------------------------------------

    function screen(root) {
      var body = h("div", { class: "stack" });
      function draw() {
        clear(body);
        if (W.current === undefined) { body.appendChild(h("div", { class: "card" }, h("p", { class: "empty" }, T("Loading…")))); return; }
        var mine = can("shift.open") || can("shift.close");
        if (W.result && W.detail[W.result] && W.detail[W.result].shift.status === "closed") {
          var r = W.detail[W.result];
          body.appendChild(h("div", { class: "card stack" }, h("div", { class: "row" }, h("h2", { class: "grow" }, T("Shift closed")),
            h("button", { type: "button", class: "small ghost", onclick: function () { W.result = null; W.redraw(); } }, T("Hide"))), detailCard(r.shift, r)));
        }
        if (mine) body.appendChild(W.current ? currentCard() : openCard());
        body.appendChild(listCard());
      }
      W.redraw = draw;
      root.appendChild(h("h1", {}, T("Shift")));
      root.appendChild(body);
      draw();
      load();
      return { update: function () { /* what is typed stays; it is drawn when something is done */ }, reload: function () { /* nothing arrives here by itself */ } };
    }

    return { shift: screen };
  };

  Shifts.pure = pure;
  if (typeof module === "object" && module.exports) module.exports = Shifts;
  else window.KitchenShifts = Shifts;
})();
