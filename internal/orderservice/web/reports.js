// The reports screen: sales by day, what sold best, what the kitchen used, what was given back and
// when it was busy, for a period, with a file to download. The figures are worked out by the service
// from a copy of its log; this file draws them. Like the other scripts, everything shown is put on
// the page as text and as elements, never as HTML.
(function () {
  "use strict";

  function pure(T) {
    function pad(n) { return String(n).padStart(2, "0"); }
    function iso(d) { return d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate()); }
    function addDays(date, n) {
      var p = date.split("-").map(Number);
      var d = new Date(Date.UTC(p[0], p[1] - 1, p[2] + n));
      return d.getUTCFullYear() + "-" + pad(d.getUTCMonth() + 1) + "-" + pad(d.getUTCDate());
    }

    // preset gives the days of a named period, counted back from today (a date like 2026-11-14).
    function preset(name, today) {
      var p = today.split("-").map(Number);
      switch (name) {
        case "today": return { from: today, to: today };
        case "yesterday": return { from: addDays(today, -1), to: addDays(today, -1) };
        case "week": return { from: addDays(today, -6), to: today };
        case "month": return { from: today.slice(0, 8) + "01", to: today };
        case "lastmonth": {
          var last = addDays(today.slice(0, 8) + "01", -1);
          return { from: last.slice(0, 8) + "01", to: last };
        }
        case "30": return { from: addDays(today, -29), to: today };
      }
      return { from: addDays(today, -6), to: today, year: p[0] };
    }

    // totals adds up the days of the sales report.
    function totals(days) {
      var t = { orders: 0, subtotal: 0, discount: 0, total: 0, cash: 0, card: 0, transfer: 0, refunds: 0, net: 0 };
      days.forEach(function (d) { Object.keys(t).forEach(function (k) { t[k] += d[k] || 0; }); });
      t.average = t.orders ? Math.round(t.total / t.orders) : 0;
      return t;
    }

    // scale gives, for each value, the height of its bar out of a height, the tallest filling it.
    // Negative values and an empty list give zero bars.
    function scale(values, height) {
      var max = 0;
      values.forEach(function (v) { if (v > max) max = v; });
      return values.map(function (v) { return max > 0 && v > 0 ? Math.max(1, Math.round(v / max * height)) : 0; });
    }

    // duration writes a number of seconds as minutes and seconds.
    function duration(s) {
      if (!s) return "–";
      var m = Math.floor(s / 60), r = s % 60;
      return m ? T("{m} min {s} s", { m: m, s: r }) : T("{s} s", { s: r });
    }

    // share is a part of a whole as a percentage with no decimals ("" for no whole).
    function share(part, whole) { return whole > 0 ? Math.round(part / whole * 100) + "%" : ""; }

    return { addDays: addDays, iso: iso, preset: preset, totals: totals, scale: scale, duration: duration, share: share };
  }

  var Reports = function (ui) {
    var h = ui.h, clear = ui.clear, T = ui.T, api = ui.api, toast = ui.toast, when = ui.when, money = ui.money;
    var p = pure(T);
    var SVGNS = "http://www.w3.org/2000/svg";

    var R = { tab: "sales", preset: "week", from: "", to: "", data: {}, status: null, problem: "", redraw: function () { /* set by the screen */ } };
    if (!R.from) { var w = p.preset("week", p.iso(new Date())); R.from = w.from; R.to = w.to; }

    function query() { return "from=" + encodeURIComponent(R.from) + "&to=" + encodeURIComponent(R.to); }
    function errText(e) { return e && e.message ? e.message : T("The request did not get an answer. Press again to retry."); }

    function load() {
      var tab = R.tab;
      R.data[tab] = undefined;
      R.redraw();
      return Promise.all([api("GET", "/api/reports/" + tab + "?" + query() + (tab === "items" ? "&limit=50" : "")), api("GET", "/api/reports/status")]).then(function (rs) {
        if (R.tab !== tab) return;
        R.data[tab] = rs[0]; R.status = rs[1]; R.problem = ""; R.redraw();
      }, function (e) { R.problem = errText(e); R.data[tab] = null; R.redraw(); });
    }

    // ---- small drawings ----------------------------------------------------------------

    function svgEl(tag, attrs) {
      var el = document.createElementNS(SVGNS, tag);
      Object.keys(attrs || {}).forEach(function (k) { el.setAttribute(k, attrs[k]); });
      return el;
    }

    // columns draws one bar for each value; label(i) says what a bar stands for, for the title that a
    // pointer shows and for a reader of the page that cannot see the picture.
    function columns(values, label, ariaLabel) {
      var W = 720, H = 140, n = values.length || 1, gap = n > 40 ? 1 : 3, bw = Math.max(2, (W - gap * (n - 1)) / n);
      var svg = svgEl("svg", { viewBox: "0 0 " + W + " " + (H + 4), class: "chart", role: "img", "aria-label": ariaLabel, preserveAspectRatio: "none" });
      var hs = p.scale(values, H);
      values.forEach(function (v, i) {
        var bar = svgEl("rect", { x: (i * (bw + gap)).toFixed(1), y: String(H - hs[i] + 2), width: bw.toFixed(1), height: String(Math.max(hs[i], 1)), rx: "1.5", class: v > 0 ? "bar" : "bar none" });
        var title = svgEl("title", {});
        title.textContent = label(i);
        bar.appendChild(title);
        svg.appendChild(bar);
      });
      return svg;
    }

    function tile(label, value, sub) {
      return h("div", { class: "tile" }, h("div", { class: "k" }, label), h("div", { class: "v" }, value), sub ? h("div", { class: "fine" }, sub) : null);
    }

    function table(head, rows, numeric) {
      return h("div", { class: "scroll" }, h("table", { class: "stackrows" },
        h("thead", {}, h("tr", {}, head.map(function (c, i) { return h("th", { class: numeric[i] ? "n" : "" }, c); }))),
        h("tbody", {}, rows.map(function (r) {
          return h("tr", {}, r.map(function (c, i) { return h("td", { class: numeric[i] ? "n" : "", "data-label": i === 0 ? null : head[i] }, c); }));
        }))));
    }

    // ---- one report each -----------------------------------------------------------------

    function salesView(d) {
      var days = d.rows, t = p.totals(days);
      var box = h("div", { class: "stack" });
      box.appendChild(h("div", { class: "tiles" },
        tile(T("Collected"), money(t.total), T("{n} orders", { n: t.orders })),
        tile(T("Given back"), money(t.refunds)),
        tile(T("Net"), money(t.net)),
        tile(T("Average order"), money(t.average)),
        tile(T("Discounts"), money(t.discount))));
      if (days.length > 1) {
        box.appendChild(h("div", { class: "card stack" }, h("h2", {}, T("Net by day")),
          columns(days.map(function (x) { return Math.max(0, x.net); }), function (i) { return days[i].date + ": " + money(days[i].net); }, T("Net sales for each day of the period")),
          h("div", { class: "row" }, h("span", { class: "fine" }, days[0].date), h("span", { class: "grow" }), h("span", { class: "fine" }, days[days.length - 1].date))));
      }
      box.appendChild(h("div", { class: "card stack" }, h("h2", {}, T("By day")),
        table([T("Day"), T("Orders"), T("Collected"), T("Cash"), T("Card"), T("Transfer"), T("Given back"), T("Net"), T("Time to make")],
          days.map(function (x) { return [x.date, String(x.orders), money(x.total), money(x.cash), money(x.card), money(x.transfer), money(x.refunds), money(x.net), p.duration(x.prep_seconds)]; })
            .concat([[T("Total"), String(t.orders), money(t.total), money(t.cash), money(t.card), money(t.transfer), money(t.refunds), money(t.net), ""]]),
          [false, true, true, true, true, true, true, true, true]),
        h("p", { class: "fine" }, T("An order counts on the day it was paid; money given back counts on the day it was given. Net is collected less given back."))));
      return box;
    }

    function itemsView(d) {
      var rows = d.rows, whole = rows.reduce(function (s, x) { return s + x.revenue; }, 0);
      if (!rows.length) return h("div", { class: "card" }, h("p", { class: "empty" }, T("Nothing was sold in this period.")));
      return h("div", { class: "card stack" }, h("h2", {}, T("Best sellers")),
        table([T("Item"), T("Category"), T("Quantity"), T("Revenue"), T("Share"), T("Orders")],
          rows.map(function (x, i) { return [(i + 1) + ". " + x.name, x.category, String(x.quantity), money(x.revenue), p.share(x.revenue, whole), String(x.orders)]; }), [false, false, true, true, true, true]),
        h("p", { class: "fine" }, T("From orders paid in the period and not cancelled; the share is of the revenue of the items listed.")));
    }

    function ingredientsView(d) {
      var rows = d.rows;
      if (!rows.length) return h("div", { class: "card" }, h("p", { class: "empty" }, T("The stock did not move in this period.")));
      return h("div", { class: "card stack" }, h("h2", {}, T("What the kitchen used")),
        table([T("Ingredient"), T("Used by orders"), T("Received"), T("Paid"), T("Written off"), T("Found by stocktakes")],
          rows.map(function (x) { return [x.ingredient, String(x.used), String(x.received), x.cost ? money(x.cost) : "", String(x.written_off), (x.adjusted > 0 ? "+" : "") + x.adjusted]; }), [false, true, true, true, true, true]),
        h("p", { class: "fine" }, T("Quantities are in the base unit of each ingredient (grams, millilitres, pieces). The Paid column is what the deliveries cost, where a price was written down.")));
    }

    function refundsView(d) {
      var rows = d.rows, sum = rows.reduce(function (s, x) { return s + x.amount; }, 0);
      if (!rows.length) return h("div", { class: "card" }, h("p", { class: "empty" }, T("Nothing was given back in this period.")));
      return h("div", { class: "card stack" }, h("div", { class: "row" }, h("h2", { class: "grow" }, T("Money given back")), h("b", { class: "money" }, money(sum))),
        table([T("When"), T("Order"), T("Kind"), T("Amount"), T("Paid by"), T("By"), T("Reason")],
          rows.map(function (x) { return [when(x.at), "#" + x.order, x.kind === "cancel" ? T("cancellation") : T("refund"), money(x.amount), x.method ? T(x.method) : "", x.by, x.reason]; }), [false, false, false, true, false, false, false]));
    }

    function hoursView(d) {
      var rows = d.rows, orders = rows.map(function (x) { return x.orders; }), n = orders.reduce(function (s, v) { return s + v; }, 0);
      if (!n) return h("div", { class: "card" }, h("p", { class: "empty" }, T("No order was paid in this period.")));
      return h("div", { class: "card stack" }, h("h2", {}, T("Orders by hour of the day")),
        columns(orders, function (i) { return T("{h}:00 – {n} orders", { h: i, n: orders[i] }); }, T("Orders paid in each hour of the day")),
        h("div", { class: "row" }, h("span", { class: "fine" }, "0:00"), h("span", { class: "grow" }), h("span", { class: "fine" }, "12:00"), h("span", { class: "grow" }), h("span", { class: "fine" }, "23:00")),
        table([T("Hour"), T("Orders"), T("Collected")], rows.filter(function (x) { return x.orders > 0; }).map(function (x) { return [x.hour + ":00", String(x.orders), money(x.total)]; }), [false, true, true]),
        h("p", { class: "fine" }, T("Hours are those of the venue's clock.")));
    }

    // ---- the screen --------------------------------------------------------------------

    var TABS = function () { return [["sales", T("Sales")], ["items", T("Best sellers")], ["ingredients", T("Ingredients")], ["refunds", T("Refunds")], ["hours", T("Busy hours")]]; };

    function screen(root) {
      var nav = h("div", { class: "subnav", role: "tablist" }), bar = h("div", { class: "card stack" }), body = h("div", { class: "stack" });
      function draw() {
        clear(nav); clear(bar); clear(body);
        TABS().forEach(function (x) {
          nav.appendChild(h("button", { type: "button", role: "tab", "aria-selected": R.tab === x[0] ? "true" : "false", onclick: function () { R.tab = x[0]; if (R.data[x[0]] === undefined) load(); else R.redraw(); } }, x[1]));
        });
        // the period
        var from = h("input", { type: "date", "aria-label": T("From") }), to = h("input", { type: "date", "aria-label": T("Until") });
        from.value = R.from; to.value = R.to;
        from.addEventListener("change", function () { R.from = from.value; R.preset = ""; });
        to.addEventListener("change", function () { R.to = to.value; R.preset = ""; });
        var presets = [["today", T("Today")], ["yesterday", T("Yesterday")], ["week", T("Last 7 days")], ["30", T("Last 30 days")], ["month", T("This month")], ["lastmonth", T("Last month")]];
        bar.appendChild(h("div", { class: "opts" }, presets.map(function (x) {
          return h("button", { type: "button", class: "small " + (R.preset === x[0] ? "" : "ghost"), onclick: function () {
            var r = p.preset(x[0], p.iso(new Date())); R.from = r.from; R.to = r.to; R.preset = x[0]; R.data = {}; load();
          } }, x[1]);
        })));
        var csv = h("a", { class: "linkbtn", href: "/api/reports/" + R.tab + "?format=csv&" + query(), download: "" }, T("Download as a spreadsheet (CSV)"));
        bar.appendChild(h("div", { class: "row" }, h("label", {}, T("From")), from, h("label", {}, T("Until")), to,
          h("button", { type: "button", class: "small", onclick: function () { R.data = {}; load(); } }, T("Show")), h("span", { class: "grow" }), csv));
        if (R.status) {
          var behind = R.status.behind > 0;
          bar.appendChild(h("p", { class: "fine" }, R.status.as_of ? T("Figures up to {when}.", { when: when(R.status.as_of) }) : T("No order has been counted yet."),
            behind ? " " + T("{n} records of the log are still being counted.", { n: R.status.behind }) : ""));
        }
        var d = R.data[R.tab];
        if (R.problem) body.appendChild(h("div", { class: "banner bad", role: "alert" }, R.problem));
        else if (d === undefined) body.appendChild(h("div", { class: "card" }, h("p", { class: "empty" }, T("Loading…"))));
        else if (d) body.appendChild(R.tab === "sales" ? salesView(d) : R.tab === "items" ? itemsView(d) : R.tab === "ingredients" ? ingredientsView(d) : R.tab === "refunds" ? refundsView(d) : hoursView(d));
      }
      R.redraw = draw;
      root.appendChild(h("h1", {}, T("Reports")));
      root.appendChild(h("div", { class: "stack" }, nav, bar, body));
      draw();
      if (R.data[R.tab] === undefined) load();
      return { update: function () { /* what is on the screen stays until it is asked again */ }, reload: function () { /* the reports are asked for, not pushed */ } };
    }

    return { reports: screen };
  };

  Reports.pure = pure;
  if (typeof module === "object" && module.exports) module.exports = Reports;
  else window.KitchenReports = Reports;
})();
