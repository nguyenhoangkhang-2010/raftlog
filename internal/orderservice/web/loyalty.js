// The points screens: for a customer, what they have earned and spent, what they may use now, and how the
// program works; for a manager, a desk where a customer's points are looked up and changed, with the reason.
// The service keeps the points in its log and works out every figure; this file draws what it says. Like the
// other scripts, everything shown is put on the page as text, never as HTML.
(function () {
  "use strict";

  function pure(T) {
    function intOf(s) {
      s = s === undefined || s === null ? "" : String(s).trim();
      return /^\d+$/.test(s) ? parseInt(s, 10) : null;
    }

    // maxUsable is the most points an order of an estimated total may use: half of it (or the share the venue
    // sets), at what a point is worth, and no more than the customer has available. It is an estimate: the
    // service decides, with the promotions, when the order is placed, and says how many it allows if it is fewer.
    function maxUsable(total, pts) {
      if (!pts || !pts.enabled || !pts.value || total <= 0 || pts.available <= 0) return 0;
      var cap = Math.floor(total * pts.max_share / 100 / pts.value);
      return Math.max(0, Math.min(cap, pts.available));
    }

    // pointsProblem checks the number of points typed for an order. "" for none (an empty box is zero).
    function pointsProblem(text, pts, total) {
      var raw = text === undefined || text === null ? "" : String(text).trim();
      if (raw === "") return "";
      var n = intOf(raw);
      if (n === null) return T("Points are a whole number.");
      if (n === 0) return "";
      var max = maxUsable(total, pts);
      if (n > pts.available) return T("You have {n} points available.", { n: pts.available });
      if (n > max) return T("This order can use at most {n} points.", { n: max });
      return "";
    }

    // MAX_ADJUST is the most points one change by a manager may give or take; the service decides again.
    var MAX_ADJUST = 10000;

    // adjustProblem checks a change of points as the manager typed it: "give" or "take", the number, and the reason.
    // "" when nothing is wrong that can be seen here; view is the points of the customer as the service gave them.
    function adjustProblem(direction, text, reason, view) {
      var n = intOf(text);
      if (n === null || n < 1) return T("Points are a whole number above zero.");
      if (n > MAX_ADJUST) return T("A change is 1 to {n} points.", { n: MAX_ADJUST });
      if (direction === "take" && view && n > Math.max(0, view.available)) {
        return T("Only {n} points can be taken off this customer.", { n: Math.max(0, view.available) });
      }
      var why = reason === undefined || reason === null ? "" : String(reason).trim();
      if (why === "") return T("Say why.");
      if (Array.from(why).length > 200) return T("The reason is at most 200 characters.");
      return "";
    }

    // adjustBody is what is sent for a change: points are positive to give and negative to take off.
    function adjustBody(commandID, customer, direction, text, reason) {
      var n = intOf(text);
      return { command_id: commandID, customer: String(customer).trim().toLowerCase(), points: direction === "take" ? -n : n, reason: String(reason).trim() };
    }

    // kindLabel says what an entry of the history was.
    function kindLabel(kind) {
      switch (kind) {
        case "earned": return T("Earned");
        case "redeemed": return T("Used");
        case "returned": return T("Given back");
        case "taken_back": return T("Taken back");
        case "adjusted": return T("Changed by the venue");
      }
      return kind;
    }

    // signed writes a number of points with its sign.
    function signed(n) { return (n > 0 ? "+" : n < 0 ? "−" : "") + Math.abs(n); }

    // rule writes how the program works in a sentence, from what the service says.
    function rule(pts, money) {
      return [
        T("Every {amount} you pay earns one point.", { amount: money(pts.earn_per) }),
        T("One point takes {amount} off an order, and points may pay for up to {share}% of it.", { amount: money(pts.value), share: pts.max_share }),
      ];
    }

    return { intOf: intOf, maxUsable: maxUsable, pointsProblem: pointsProblem, kindLabel: kindLabel, signed: signed, rule: rule,
      adjustProblem: adjustProblem, adjustBody: adjustBody, MAX_ADJUST: MAX_ADJUST };
  }

  var Loyalty = function (ui) {
    var h = ui.h, clear = ui.clear, T = ui.T, api = ui.api, when = ui.when, money = ui.money, run = ui.run, toast = ui.toast, fieldOf = ui.fieldOf;
    var p = pure(T);
    var W = { data: undefined, problem: "", redraw: function () { /* set by the screen */ } };

    function load() {
      return api("GET", "/api/points").then(function (d) { W.data = d; W.problem = ""; W.redraw(); },
        function (e) { W.problem = e && e.message ? e.message : T("The request did not get an answer. Press again to retry."); W.data = null; W.redraw(); });
    }

    function tile(label, value, sub) {
      return h("div", { class: "tile" }, h("div", { class: "k" }, label), h("div", { class: "v" }, value), sub ? h("div", { class: "fine" }, sub) : null);
    }

    // historyList draws what happened to the points, newest first. A change by a manager has no order: it shows
    // its reason, and, for staff, who made it.
    function historyList(d, staff) {
      return h("ul", { class: "rowlist" }, d.history.slice().reverse().map(function (e) {
        return h("li", { class: "rowitem" },
          h("div", { class: "grow" }, h("b", {}, p.kindLabel(e.kind)), e.number ? [" ", T("Order {n}", { n: "#" + e.number })] : null,
            e.reason ? h("div", {}, e.reason) : null,
            h("div", { class: "fine" }, when(e.at), staff && e.by ? [" · ", T("by {who}", { who: e.by })] : null)),
          h("b", { class: "money " + (e.points > 0 ? "ok-text" : "") }, p.signed(e.points)));
      }));
    }

    function view(d) {
      if (!d.enabled) return h("div", { class: "card" }, h("p", { class: "empty" }, T("This venue has no points program.")));
      var box = h("div", { class: "stack" });
      box.appendChild(h("div", { class: "tiles" },
        tile(T("You can use"), String(d.available), T("worth {amount}", { amount: money(d.available * d.value) })),
        tile(T("You own"), String(d.balance), d.held ? T("{n} are set aside for orders not paid yet", { n: d.held }) : ""),
        tile(T("Earned in all"), String(d.earned)),
        tile(T("Used in all"), String(d.spent))));
      box.appendChild(h("div", { class: "card stack" }, h("h2", {}, T("How points work")),
        p.rule(d, money).map(function (s) { return h("p", {}, s); }),
        h("p", { class: "fine" }, T("Points are used when you place an order, and are spent when it is paid. If an order is cancelled the points are given back, and the ones it earned are taken back."))));
      var hist = h("div", { class: "card stack" }, h("h2", {}, T("What happened to your points")));
      if (!d.history.length) hist.appendChild(h("p", { class: "empty" }, T("No points yet. Order, and they come when the order is paid.")));
      else {
        hist.appendChild(historyList(d, false));
        if (d.dropped) hist.appendChild(h("p", { class: "fine" }, T("{n} older entries are not shown.", { n: d.dropped })));
      }
      box.appendChild(hist);
      return box;
    }

    function screen(root) {
      var body = h("div", { class: "stack" });
      function draw() {
        clear(body);
        if (W.problem) body.appendChild(h("div", { class: "banner bad", role: "alert" }, W.problem));
        else if (W.data === undefined) body.appendChild(h("div", { class: "card" }, h("p", { class: "empty" }, T("Loading…"))));
        else if (W.data) body.appendChild(view(W.data));
      }
      W.redraw = draw;
      root.appendChild(h("h1", {}, T("Points")));
      root.appendChild(body);
      draw();
      load();
      return { update: function () { /* the points are asked for, not pushed */ }, reload: function () { load(); } };
    }

    // ---- the desk of a manager -------------------------------------------------------------------

    var D = { login: "", data: undefined, problem: "", redraw: function () { /* set by the screen */ } };

    function lookup(login) {
      login = String(login).trim().toLowerCase();
      if (!login) { D.problem = T("Type the login name of a customer."); D.data = undefined; D.redraw(); return Promise.resolve(); }
      D.login = login;
      return api("GET", "/api/points?customer=" + encodeURIComponent(login)).then(function (d) { D.data = d; D.problem = ""; D.redraw(); },
        function (e) { D.problem = e && e.message ? e.message : T("The request did not get an answer. Press again to retry."); D.data = undefined; D.redraw(); });
    }

    function changeForm(d) {
      var what = h("select", { id: "pc-what", "aria-label": T("What to do") },
        h("option", { value: "give" }, T("Give points")), h("option", { value: "take" }, T("Take points off")));
      var n = h("input", { type: "text", id: "pc-n", inputmode: "numeric", autocomplete: "off", placeholder: "0" });
      var why = h("input", { type: "text", id: "pc-why", maxlength: "200", autocomplete: "off" });
      var problem = h("p", { class: "problem", role: "alert" });
      var btn = h("button", { type: "button", class: "primary" }, T("Save the change"));
      btn.addEventListener("click", function () {
        var bad = p.adjustProblem(what.value, n.value, why.value, d);
        problem.textContent = bad;
        if (bad) return;
        run("points:" + D.login + ":" + what.value + ":" + n.value + ":" + why.value, btn, function (id) {
          return api("POST", "/api/points/adjust", p.adjustBody(id, D.login, what.value, n.value, why.value));
        }).then(function (v) { if (v) { toast(T("Points changed."), "ok"); D.data = v; D.redraw(); } });
      });
      return h("div", { class: "card stack" }, h("h2", {}, T("Change points")),
        h("div", { class: "erow c4" }, fieldOf("pc-what", T("What to do"), what), fieldOf("pc-n", T("Points"), n), fieldOf("pc-why", T("Reason"), why), h("div", {}, btn)),
        h("p", { class: "fine" }, T("The reason is kept with the change, and the customer sees it. A change of at most {n} points at a time.", { n: p.MAX_ADJUST })), problem);
    }

    function deskView(d) {
      if (!d.enabled) return h("div", { class: "card" }, h("p", { class: "empty" }, T("This venue has no points program.")));
      var box = h("div", { class: "stack" });
      box.appendChild(h("div", { class: "tiles" },
        tile(T("Available now"), String(d.available), T("worth {amount}", { amount: money(Math.max(0, d.available) * d.value) })),
        tile(T("Balance"), String(d.balance), d.held ? T("{n} are set aside for orders not paid yet", { n: d.held }) : ""),
        tile(T("Earned in all"), String(d.earned)),
        tile(T("Used in all"), String(d.spent)),
        tile(T("Changed by managers"), p.signed(d.adjusted))));
      box.appendChild(changeForm(d));
      var hist = h("div", { class: "card stack" }, h("h2", {}, T("What happened to the points")));
      if (!d.history.length) hist.appendChild(h("p", { class: "empty" }, T("This customer has no points yet.")));
      else {
        hist.appendChild(historyList(d, true));
        if (d.dropped) hist.appendChild(h("p", { class: "fine" }, T("{n} older entries are not shown.", { n: d.dropped })));
      }
      box.appendChild(hist);
      return box;
    }

    function desk(root) {
      var body = h("div", { class: "stack" });
      var login = h("input", { type: "text", id: "pd-login", autocomplete: "off", autocapitalize: "none", spellcheck: "false", maxlength: "32", value: D.login });
      var go = h("button", { type: "submit", class: "primary" }, T("Look up"));
      function draw() {
        clear(body);
        if (D.problem) body.appendChild(h("div", { class: "banner bad", role: "alert" }, D.problem));
        if (D.data) body.appendChild(deskView(D.data));
      }
      D.redraw = draw;
      root.appendChild(h("h1", {}, T("Customer points")));
      root.appendChild(h("form", { class: "card stack", onsubmit: function (e) { e.preventDefault(); lookup(login.value); } },
        fieldOf("pd-login", T("Login name of the customer"), login),
        h("p", { class: "fine" }, T("Type a customer's login name to see their points and change them.")), go));
      root.appendChild(body);
      draw();
      if (D.login && D.data === undefined) lookup(D.login);
      return { update: function () { /* asked for, not pushed */ }, reload: function () { if (D.login) lookup(D.login); } };
    }

    return { points: screen, pointsdesk: desk };
  };

  Loyalty.pure = pure;
  if (typeof module === "object" && module.exports) module.exports = Loyalty;
  else window.KitchenLoyalty = Loyalty;
})();
