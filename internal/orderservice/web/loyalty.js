// The points screen: what a customer has earned and spent, what they may use now, and how the program works.
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

    // kindLabel says what an entry of the history was.
    function kindLabel(kind) {
      switch (kind) {
        case "earned": return T("Earned");
        case "redeemed": return T("Used");
        case "returned": return T("Given back");
        case "taken_back": return T("Taken back");
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

    return { intOf: intOf, maxUsable: maxUsable, pointsProblem: pointsProblem, kindLabel: kindLabel, signed: signed, rule: rule };
  }

  var Loyalty = function (ui) {
    var h = ui.h, clear = ui.clear, T = ui.T, api = ui.api, when = ui.when, money = ui.money;
    var p = pure(T);
    var W = { data: undefined, problem: "", redraw: function () { /* set by the screen */ } };

    function load() {
      return api("GET", "/api/points").then(function (d) { W.data = d; W.problem = ""; W.redraw(); },
        function (e) { W.problem = e && e.message ? e.message : T("The request did not get an answer. Press again to retry."); W.data = null; W.redraw(); });
    }

    function tile(label, value, sub) {
      return h("div", { class: "tile" }, h("div", { class: "k" }, label), h("div", { class: "v" }, value), sub ? h("div", { class: "fine" }, sub) : null);
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
        hist.appendChild(h("ul", { class: "rowlist" }, d.history.slice().reverse().map(function (e) {
          return h("li", { class: "rowitem" },
            h("div", { class: "grow" }, h("b", {}, p.kindLabel(e.kind)), " ", T("Order {n}", { n: "#" + e.number }), h("div", { class: "fine" }, when(e.at))),
            h("b", { class: "money " + (e.points > 0 ? "ok-text" : "") }, p.signed(e.points)));
        })));
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

    return { points: screen };
  };

  Loyalty.pure = pure;
  if (typeof module === "object" && module.exports) module.exports = Loyalty;
  else window.KitchenLoyalty = Loyalty;
})();
