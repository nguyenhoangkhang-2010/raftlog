// The warehouse screen: what is in stock, in lots with their dates and prices; receiving a delivery
// from a supplier; the history of each ingredient; what was wasted; stocktakes; and the suppliers
// and the names and units of the ingredients. What is shown is what the service keeps from the log
// and the database; this file only draws it and sends what the person types. Like the other
// scripts, everything shown is put on the page as text, never as HTML.
(function () {
  "use strict";

  // pure holds what does not touch the page, so that the tests run it (warehouse.test.js).
  function pure(T) {
    function str(v) { return v === undefined || v === null ? "" : String(v); }
    function intOf(s) { s = str(s).trim(); return /^-?\d+$/.test(s) ? parseInt(s, 10) : null; }
    function bad(msg) { return { err: msg }; }
    var DATE = /^\d{4}-\d{2}-\d{2}$/;

    // validDate says that a text is a real calendar date like 2026-12-31.
    function validDate(s) {
      if (!DATE.test(s)) return false;
      var p = s.split("-").map(Number);
      var d = new Date(Date.UTC(p[0], p[1] - 1, p[2]));
      return d.getUTCFullYear() === p[0] && d.getUTCMonth() === p[1] - 1 && d.getUTCDate() === p[2];
    }

    // addDays gives the date n days after a date (both like 2026-12-31), without the time zone
    // getting in the way.
    function addDays(date, n) {
      var p = date.split("-").map(Number);
      var d = new Date(Date.UTC(p[0], p[1] - 1, p[2] + n));
      var z = function (x) { return String(x).padStart(2, "0"); };
      return d.getUTCFullYear() + "-" + z(d.getUTCMonth() + 1) + "-" + z(d.getUTCDate());
    }

    // receiptBody checks a delivery as it was typed and builds what the service takes. A line with
    // nothing in it is left out.
    function receiptBody(draft) {
      var lines = [];
      for (var i = 0; i < draft.lines.length; i++) {
        var l = draft.lines[i];
        var blank = !str(l.ingredient).trim() && !str(l.qty).trim() && !str(l.cost).trim() && !str(l.expires).trim();
        if (blank) continue;
        var n = i + 1;
        if (!str(l.ingredient).trim()) return bad(T("Line {n}: choose an ingredient.", { n: n }));
        var q = intOf(l.qty);
        if (q === null || q < 1) return bad(T("Line {n}: the quantity is a whole number above zero.", { n: n }));
        var cost = str(l.cost).trim() === "" ? 0 : intOf(l.cost);
        if (cost === null || cost < 0) return bad(T("Line {n}: the price is a whole number of đồng, zero or more.", { n: n }));
        var exp = str(l.expires).trim();
        if (exp && !validDate(exp)) return bad(T("Line {n}: the date is like 2026-12-31.", { n: n }));
        lines.push({ ingredient: l.ingredient.trim(), qty: q, total_cost: cost, expires: exp });
      }
      if (!lines.length) return bad(T("Add at least one line to the delivery."));
      return { body: { supplier: str(draft.supplier), document: str(draft.document).trim(), lines: lines } };
    }

    // milliToDong rounds thousandths of a đồng to whole đồng.
    function milliToDong(m) { return Math.round(m / 1000); }

    // unitCost writes the price of one unit: whole đồng when it is, else with decimals.
    function unitCost(milli) {
      if (!milli) return "";
      var d = milli / 1000;
      var s = Math.abs(d - Math.round(d)) < 0.005 ? String(Math.round(d)) : d.toFixed(2).replace(/0+$/, "").replace(/\.$/, "");
      return s;
    }

    // soonest is the first date at which a lot of the row expires, or "".
    function soonest(row) {
      var best = "";
      (row.lots || []).forEach(function (l) { if (l.expires_at && (!best || l.expires_at < best)) best = l.expires_at; });
      return best;
    }

    // alerts counts the ingredients that need attention.
    function alerts(rows) {
      var a = { low: 0, soon: 0, expired: 0 };
      rows.forEach(function (r) { if (r.low) a.low++; if (r.soon) a.soon++; if (r.expired) a.expired++; });
      return a;
    }

    // variance is what a count found against what the stock said then.
    function variance(line) { return line.counted - line.expected; }

    // lastDay is the last day a lot may be used, from the moment it stops being usable (the start
    // of the day after), in the browser's own zone.
    function lastDay(iso) {
      var d = new Date(new Date(iso).getTime() - 1);
      var z = function (x) { return String(x).padStart(2, "0"); };
      return d.getFullYear() + "-" + z(d.getMonth() + 1) + "-" + z(d.getDate());
    }

    return { validDate: validDate, addDays: addDays, receiptBody: receiptBody, milliToDong: milliToDong, unitCost: unitCost,
      soonest: soonest, alerts: alerts, variance: variance, lastDay: lastDay, intOf: intOf };
  }

  var Warehouse = function (ui) {
    var h = ui.h, clear = ui.clear, T = ui.T, api = ui.api, run = ui.run, toast = ui.toast, fieldOf = ui.fieldOf;
    var twoStep = ui.twoStep, when = ui.when, money = ui.money;
    var p = pure(T);

    // What the screen keeps between drawings (a change of language draws it again).
    var W = { data: null, tab: "overview", open: {}, ledger: null, ledgerIng: "", receipts: null, waste: null, wasteFrom: "", wasteTo: "",
      stocktake: undefined, stocktakes: null, suppliers: null, draft: null, writeoff: null, problem: "", redraw: function () { /* set by the screen */ } };

    function perms() { return ui.S.me.permissions; }
    function can(x) { return !!perms()[x]; }
    function hasDB() { return !!(W.data && W.data.infos); }

    function newDraft() { return { supplier: "", document: "", lines: [{ ingredient: "", qty: "", cost: "", expires: "" }] }; }

    function errText(e) { return e && e.message ? e.message : T("The request did not get an answer. Press again to retry."); }
    function fail(e) { toast(errText(e), "bad"); }

    function load() {
      return api("GET", "/api/warehouse").then(function (d) { W.data = d; W.problem = ""; W.redraw(); }, function (e) { W.problem = errText(e); W.redraw(); });
    }
    function nameOf(ing) {
      var i = W.data && W.data.infos && W.data.infos[ing];
      return i && i.name ? i.name : ing;
    }
    function unitOf(ing) {
      var i = W.data && W.data.infos && W.data.infos[ing];
      return i && i.unit ? i.unit : "";
    }
    function qtyText(ing, n) { var u = unitOf(ing); return String(n) + (u ? " " + u : ""); }
    function supplierName(id) {
      if (!id) return "";
      var s = ((W.data && W.data.suppliers) || []).filter(function (x) { return x.id === id; })[0];
      return s ? s.name : id;
    }
    function ago(iso) { return when(iso); }
    function dateOf(iso) { return p.lastDay(iso); }

    function kindLabel(k) {
      switch (k) {
        case "receive": return T("Received");
        case "consume": return T("Used by an order");
        case "release": return T("Given back by an order");
        case "write_off": return T("Written off");
        case "count": return T("Counted");
        case "adjust": return T("Adjusted");
      }
      return k;
    }
    function causeLabel(c) {
      switch (c) {
        case "spoiled": return T("Spoiled");
        case "expired": return T("Expired");
        case "spilled": return T("Spilled or dropped");
        case "other": return T("Other");
        case "stocktake": return T("Missing at a stocktake");
      }
      return c || T("Other");
    }
    function costCell(milli) { return milli ? money(p.milliToDong(milli)) : ""; }

    // ---- overview ----------------------------------------------------------------------

    function alertBar(rows) {
      var a = p.alerts(rows);
      var items = [];
      if (a.expired) items.push(h("span", { class: "badge soldout" }, T("{n} with an expired lot", { n: a.expired })));
      if (a.soon) items.push(h("span", { class: "badge low" }, T("{n} with a lot that expires soon", { n: a.soon })));
      if (a.low) items.push(h("span", { class: "badge low" }, T("{n} running low", { n: a.low })));
      if (!items.length) return h("p", { class: "fine" }, T("Nothing needs attention."));
      return h("div", { class: "row" }, items);
    }

    function writeOffForm(row, lot) {
      var qty = h("input", { type: "text", inputmode: "numeric", "aria-label": T("Quantity"), placeholder: T("Quantity"), value: lot ? String(lot.qty) : "" });
      var cause = h("select", { "aria-label": T("Cause") }, ["spoiled", "expired", "spilled", "other"].map(function (c) {
        return h("option", { value: c, selected: (lot && lot.state === "expired" ? "expired" : "spoiled") === c || null }, causeLabel(c));
      }));
      var reason = h("input", { type: "text", maxlength: "200", "aria-label": T("Reason"), placeholder: T("Reason"), autocomplete: "off" });
      var problem = h("p", { class: "problem", role: "alert" });
      var btn = h("button", { type: "button", class: "small" }, T("Write off"));
      btn.addEventListener("click", function () {
        var q = p.intOf(qty.value);
        if (q === null || q < 1) { problem.textContent = T("The quantity is a whole number above zero."); return; }
        if (!reason.value.trim()) { problem.textContent = T("Say why."); return; }
        problem.textContent = "";
        run("writeoff:" + row.ingredient + ":" + (lot ? lot.id : "") + ":" + q + ":" + cause.value + ":" + reason.value, btn, function (id) {
          var body = { command_id: id, ingredient: row.ingredient, qty: q, reason: reason.value.trim(), cause: cause.value };
          if (lot) body.lot = lot.id;
          return api("POST", "/api/stock/write-off", body);
        }).then(function (v) { if (v) { toast(T("Written off."), "ok"); W.writeoff = null; load(); } });
      });
      return h("div", { class: "sub stack" }, h("div", { class: "erow c4" }, qty, cause, reason, h("div", {}, btn)), problem);
    }

    function lotsTable(row) {
      var costs = W.data.costs;
      return h("div", { class: "scroll" }, h("table", { class: "stackrows" },
        h("thead", {}, h("tr", {}, h("th", {}, T("Lot")), h("th", { class: "n" }, T("Left")), h("th", {}, T("Expires")), h("th", {}, T("From")),
          costs ? h("th", { class: "n" }, T("Price of one")) : null, h("th", {}, ""))),
        h("tbody", {}, row.lots.map(function (l) {
          var key = row.ingredient + "/" + l.id;
          var state = l.state === "expired" ? h("span", { class: "badge soldout" }, T("expired")) : l.state === "soon" ? h("span", { class: "badge low" }, T("expires soon")) : null;
          return [h("tr", {},
            h("td", {}, h("code", { class: "code" }, l.document || l.id.slice(-8)), h("div", { class: "fine" }, ago(l.received_at))),
            h("td", { class: "n", "data-label": T("Left") }, qtyText(row.ingredient, l.qty), h("div", { class: "fine" }, T("of {n}", { n: l.received }))),
            h("td", { "data-label": T("Expires") }, l.expires_at ? dateOf(l.expires_at) : T("no date"), " ", state),
            h("td", { "data-label": T("From") }, supplierName(l.supplier)),
            costs ? h("td", { class: "n", "data-label": T("Price of one") }, l.unit_cost_milli ? p.unitCost(l.unit_cost_milli) + "đ" : "") : null,
            h("td", {}, can("stock.adjust") ? h("button", { type: "button", class: "small ghost", onclick: function () { W.writeoff = W.writeoff === key ? null : key; W.redraw(); } }, T("Write off")) : null)),
            W.writeoff === key ? h("tr", { class: "detail" }, h("td", { colspan: costs ? "6" : "5" }, writeOffForm(row, l))) : null];
        }))));
    }

    function overview() {
      var d = W.data, box = h("div", { class: "stack" });
      box.appendChild(h("div", { class: "card stack" }, h("h2", {}, T("Needs attention")), alertBar(d.rows)));
      var costs = d.costs;
      var rows = d.rows.map(function (r) {
        var open = !!W.open[r.ingredient];
        var flags = [];
        if (r.expired) flags.push(h("span", { class: "badge soldout" }, T("expired")));
        if (r.soon) flags.push(h("span", { class: "badge low" }, T("expires soon")));
        if (r.low) flags.push(h("span", { class: "badge low" }, T("low")));
        if (!r.qty) flags.push(h("span", { class: "badge soldout" }, T("none")));
        var next = p.soonest(r);
        return [h("tr", { class: open ? "open" : "" },
          h("td", {}, h("b", {}, nameOf(r.ingredient)), nameOf(r.ingredient) !== r.ingredient ? h("small", { class: "fine" }, " " + r.ingredient) : null),
          h("td", { class: "n", "data-label": T("In stock") }, qtyText(r.ingredient, r.qty), r.qty !== r.usable ? h("div", { class: "fine" }, T("{n} usable", { n: r.usable })) : null),
          h("td", { "data-label": T("Alerts") }, flags),
          h("td", { "data-label": T("Next expiry") }, next ? dateOf(next) : "–"),
          costs ? h("td", { class: "n", "data-label": T("Worth") }, r.value_milli ? money(p.milliToDong(r.value_milli)) : "", r.unpriced ? h("div", { class: "fine" }, T("{n} with no price", { n: r.unpriced })) : null) : null,
          h("td", {}, h("div", { class: "acts" },
            h("button", { type: "button", class: "small ghost", onclick: function () { W.open[r.ingredient] = !open; W.redraw(); } }, open ? T("Hide lots") : T("Lots ({n})", { n: r.lots.length })),
            h("button", { type: "button", class: "small ghost", onclick: function () { W.tab = "history"; W.ledgerIng = r.ingredient; loadLedger(); W.redraw(); } }, T("History"))))),
          open ? h("tr", { class: "detail" }, h("td", { colspan: costs ? "6" : "5" },
            r.lots.length ? lotsTable(r) : h("p", { class: "fine" }, T("No lots: nothing of this in stock.")),
            can("stock.adjust") && r.qty ? [h("div", {}, h("button", { type: "button", class: "small ghost", onclick: function () { W.writeoff = W.writeoff === r.ingredient ? null : r.ingredient; W.redraw(); } }, T("Write off from the oldest lots"))),
              W.writeoff === r.ingredient ? writeOffForm(r, null) : null] : null)) : null];
      });
      box.appendChild(h("div", { class: "card scroll" }, h("h2", {}, T("Stock")),
        h("table", { class: "stackrows" }, h("thead", {}, h("tr", {}, h("th", {}, T("Ingredient")), h("th", { class: "n" }, T("In stock")), h("th", {}, T("Alerts")), h("th", {}, T("Next expiry")),
          costs ? h("th", { class: "n" }, T("Worth")) : null, h("th", {}, ""))), h("tbody", {}, rows)),
        h("p", { class: "fine" }, T("The lot that expires first is used first. A lot that has expired is not used for an order; it stays here until it is written off."))));
      return box;
    }

    // ---- receiving ---------------------------------------------------------------------

    function receiveTab() {
      var d = W.data;
      if (!W.draft) W.draft = newDraft();
      var draft = W.draft;
      var box = h("div", { class: "card stack" });
      var sup = h("select", { "aria-label": T("Supplier") }, [h("option", { value: "" }, T("No supplier named"))].concat(
        (d.suppliers || []).filter(function (s) { return s.active; }).map(function (s) { return h("option", { value: s.id, selected: draft.supplier === s.id || null }, s.name); })));
      sup.addEventListener("change", function () { draft.supplier = sup.value; });
      var doc = h("input", { type: "text", maxlength: "40", autocomplete: "off", "aria-label": T("Document number"), placeholder: T("Invoice or delivery note number") });
      doc.value = draft.document;
      doc.addEventListener("input", function () { draft.document = doc.value; });
      box.appendChild(h("h2", {}, T("Receive a delivery")));
      box.appendChild(h("div", { class: "cols two-eq" }, fieldOf("rc-sup", T("Supplier"), Object.assign(sup, { id: "rc-sup" })), fieldOf("rc-doc", T("Document"), Object.assign(doc, { id: "rc-doc" }))));
      var datalist = h("datalist", { id: "wh-ingredients" }, d.rows.map(function (r) { return h("option", { value: r.ingredient }, nameOf(r.ingredient)); }));
      box.appendChild(datalist);
      var lines = h("div", { class: "stack" });
      draft.lines.forEach(function (l, i) {
        function bind(key, attrs) {
          var el = h("input", Object.assign({ type: "text", autocomplete: "off" }, attrs));
          el.value = l[key];
          el.addEventListener("input", function () { l[key] = el.value; });
          return el;
        }
        var ing = bind("ingredient", { list: "wh-ingredients", "aria-label": T("Ingredient"), placeholder: T("Ingredient"), autocapitalize: "none" });
        var expires = bind("expires", { type: "date", "aria-label": T("Last day it may be used") });
        ing.addEventListener("change", function () {
          var info = d.infos && d.infos[l.ingredient.trim()];
          if (info && info.shelf_days && !l.expires) {
            var t = new Date(), z = function (x) { return String(x).padStart(2, "0"); };
            l.expires = p.addDays(t.getFullYear() + "-" + z(t.getMonth() + 1) + "-" + z(t.getDate()), info.shelf_days);
            expires.value = l.expires;
          }
        });
        lines.appendChild(h("div", { class: "sub stack" },
          h("div", { class: "erow c3" }, ing, bind("qty", { inputmode: "numeric", "aria-label": T("Quantity"), placeholder: T("Quantity") }),
            h("button", { type: "button", class: "small ghost", "aria-label": T("Remove"), title: T("Remove"), onclick: function () { draft.lines.splice(i, 1); if (!draft.lines.length) draft.lines.push({ ingredient: "", qty: "", cost: "", expires: "" }); W.redraw(); } }, "×")),
          h("div", { class: "cols two-eq" },
            bind("cost", { inputmode: "numeric", "aria-label": T("Price of the whole line (đồng)"), placeholder: T("Price of the whole line (đồng)") }),
            expires)));
      });
      box.appendChild(lines);
      box.appendChild(h("div", {}, h("button", { type: "button", class: "small ghost", onclick: function () { draft.lines.push({ ingredient: "", qty: "", cost: "", expires: "" }); W.redraw(); } }, T("Add a line"))));
      var problem = h("p", { class: "problem", role: "alert" }, W.problem === "draft" ? "" : "");
      var btn = h("button", { type: "button", class: "primary" }, T("Receive the delivery"));
      btn.addEventListener("click", function () {
        var r = p.receiptBody(draft);
        if (r.err) { problem.textContent = r.err; return; }
        problem.textContent = "";
        run("receive:" + JSON.stringify(r.body), btn, function (id) {
          return api("POST", "/api/warehouse/receive", Object.assign({ command_id: id }, r.body));
        }).then(function (v) {
          if (!v) return;
          toast(T("Delivery received."), "ok");
          W.draft = newDraft(); W.data = v; W.receipts = null; W.redraw();
        });
      });
      box.appendChild(problem);
      box.appendChild(h("div", { class: "row" }, btn));
      box.appendChild(h("p", { class: "fine" }, T("Each line makes a lot. Without a price the lot has no value; without a date it never expires. The same delivery sent twice is received once.")));
      return box;
    }

    // ---- history -----------------------------------------------------------------------

    function loadLedger() {
      if (!W.ledgerIng) { W.ledger = null; return Promise.resolve(); }
      return api("GET", "/api/warehouse/ledger?limit=200&ingredient=" + encodeURIComponent(W.ledgerIng)).then(function (v) { W.ledger = v; W.redraw(); }, fail);
    }
    function loadReceipts() {
      return api("GET", "/api/warehouse/receipts?limit=30").then(function (v) { W.receipts = v.receipts; W.redraw(); }, fail);
    }

    function historyTab() {
      var d = W.data, box = h("div", { class: "stack" });
      var sel = h("select", { "aria-label": T("Ingredient") }, [h("option", { value: "" }, T("Choose an ingredient"))].concat(d.rows.map(function (r) {
        return h("option", { value: r.ingredient, selected: W.ledgerIng === r.ingredient || null }, nameOf(r.ingredient));
      })));
      sel.addEventListener("change", function () { W.ledgerIng = sel.value; W.ledger = null; loadLedger(); W.redraw(); });
      var card = h("div", { class: "card stack" }, h("h2", {}, T("Movements of an ingredient")), h("div", { class: "row" }, sel));
      if (W.ledger && W.ledger.ingredient === W.ledgerIng && W.ledgerIng) {
        var costs = d.costs;
        if (!W.ledger.entries.length) card.appendChild(h("p", { class: "empty" }, T("Nothing has moved.")));
        else card.appendChild(h("div", { class: "scroll" }, h("table", { class: "stackrows" },
          h("thead", {}, h("tr", {}, h("th", {}, T("When")), h("th", {}, T("What")), h("th", { class: "n" }, T("Change")), h("th", { class: "n" }, T("Left")), h("th", {}, T("By")), h("th", {}, T("Why")), costs ? h("th", { class: "n" }, T("Value")) : null)),
          h("tbody", {}, W.ledger.entries.map(function (e) {
            var why = e.kind === "consume" || e.kind === "release" ? T("Order {ref}", { ref: String(e.ref).replace(/^ord-/, "") }) :
              [e.cause ? causeLabel(e.cause) : "", e.reason, e.document ? "#" + e.document : "", e.supplier ? supplierName(e.supplier) : ""].filter(Boolean).join(" · ");
            return h("tr", {}, h("td", { "data-label": T("When") }, when(e.at)), h("td", { "data-label": T("What") }, kindLabel(e.kind)),
              h("td", { class: "n", "data-label": T("Change") }, (e.delta > 0 ? "+" : "") + e.delta), h("td", { class: "n", "data-label": T("Left") }, String(e.after)),
              h("td", { "data-label": T("By") }, e.by), h("td", { class: "wide", "data-label": T("Why") }, why), costs ? h("td", { class: "n", "data-label": T("Value") }, costCell(e.value_milli)) : null);
          })))));
        if (W.ledger.truncated) card.appendChild(h("p", { class: "fine" }, T("Only the latest {n} movements of an ingredient are kept on this screen; the log has them all.", { n: d.ledger_cap })));
      }
      box.appendChild(card);

      var rc = h("div", { class: "card stack" }, h("h2", {}, T("Latest deliveries")));
      if (!W.receipts) { rc.appendChild(h("p", { class: "empty" }, T("Loading…"))); loadReceipts(); }
      else if (!W.receipts.length) rc.appendChild(h("p", { class: "empty" }, T("No delivery yet.")));
      else rc.appendChild(h("ul", { class: "rowlist" }, W.receipts.map(function (r) {
        return h("li", { class: "rowitem" }, h("div", { class: "grow" },
          h("b", {}, r.document ? "#" + r.document : T("No document")), r.supplier ? h("small", { class: "fine" }, " " + supplierName(r.supplier)) : null,
          h("div", { class: "fine" }, r.lines.map(function (l) { return nameOf(l.ingredient) + " " + qtyText(l.ingredient, l.qty); }).join(", "))),
          h("div", { class: "fine" }, when(r.at) + " · " + r.by),
          d.costs ? h("div", { class: "money" }, money(p.milliToDong(r.lines.reduce(function (s, l) { return s + (l.value_milli || 0); }, 0)))) : null);
      })));
      box.appendChild(rc);
      return box;
    }

    // ---- waste -------------------------------------------------------------------------

    function loadWaste() {
      var q = (W.wasteFrom ? "from=" + W.wasteFrom + "&" : "") + (W.wasteTo ? "to=" + W.wasteTo : "");
      return api("GET", "/api/warehouse/waste?" + q).then(function (v) { W.waste = v; W.wasteFrom = v.from; W.wasteTo = v.to; W.redraw(); }, fail);
    }

    function wasteTab() {
      var d = W.data;
      var from = h("input", { type: "date", "aria-label": T("From") }), to = h("input", { type: "date", "aria-label": T("Until") });
      from.value = W.wasteFrom; to.value = W.wasteTo;
      from.addEventListener("change", function () { W.wasteFrom = from.value; });
      to.addEventListener("change", function () { W.wasteTo = to.value; });
      var card = h("div", { class: "card stack" }, h("h2", {}, T("Waste")),
        h("div", { class: "row" }, h("label", {}, T("From")), from, h("label", {}, T("Until")), to, h("button", { type: "button", class: "small", onclick: function () { loadWaste(); } }, T("Show"))));
      if (!W.waste) { card.appendChild(h("p", { class: "empty" }, T("Loading…"))); loadWaste(); return card; }
      var w = W.waste;
      if (!w.rows.length) card.appendChild(h("p", { class: "empty" }, T("Nothing was written off in this period.")));
      else {
        var byCause = {};
        w.rows.forEach(function (r) { var c = byCause[r.cause] || (byCause[r.cause] = { qty: 0, value: 0 }); c.qty++; c.value += r.value_milli || 0; });
        card.appendChild(h("div", { class: "tiles" }, Object.keys(byCause).sort().map(function (c) {
          return h("div", { class: "tile" }, h("div", { class: "k" }, causeLabel(c)), h("div", { class: "v" }, d.costs ? money(p.milliToDong(byCause[c].value)) : String(byCause[c].qty)),
            d.costs ? null : h("div", { class: "fine" }, T("ingredients")));
        })));
        card.appendChild(h("div", { class: "scroll" }, h("table", { class: "stackrows" },
          h("thead", {}, h("tr", {}, h("th", {}, T("Ingredient")), h("th", {}, T("Cause")), h("th", { class: "n" }, T("Quantity")), w.costs ? h("th", { class: "n" }, T("Value")) : null)),
          h("tbody", {}, w.rows.map(function (r) {
            return h("tr", {}, h("td", { "data-label": T("Ingredient") }, nameOf(r.ingredient)), h("td", { "data-label": T("Cause") }, causeLabel(r.cause)),
              h("td", { class: "n", "data-label": T("Quantity") }, qtyText(r.ingredient, r.qty)), w.costs ? h("td", { class: "n", "data-label": T("Value") }, costCell(r.value_milli)) : null);
          })))));
        if (!w.costs) card.appendChild(h("p", { class: "fine" }, T("What it cost is shown to the storekeeper and the manager.")));
      }
      if (w.truncated) card.appendChild(h("p", { class: "fine" }, T("The history of some ingredient does not reach back to the first day: the screen keeps the latest {n} movements of each.", { n: d.ledger_cap })));
      return card;
    }

    // ---- stocktake ---------------------------------------------------------------------

    function loadStocktake() {
      return Promise.all([api("GET", "/api/stocktakes/current"), api("GET", "/api/stocktakes")]).then(function (rs) {
        W.stocktake = rs[0].stocktake; W.stocktakes = rs[1].stocktakes; W.redraw();
      }, fail);
    }
    function setStocktake(v) { W.stocktake = v.stocktake; W.redraw(); }

    function stocktakeTab() {
      var d = W.data, box = h("div", { class: "stack" });
      if (W.stocktake === undefined) { loadStocktake(); return h("div", { class: "card" }, h("p", { class: "empty" }, T("Loading…"))); }
      var st = W.stocktake;
      if (!st) {
        var note = h("input", { type: "text", maxlength: "200", autocomplete: "off", placeholder: T("A note, such as the month"), "aria-label": T("Note") });
        var start = h("button", { type: "button", class: "primary" }, T("Start a stocktake"));
        start.addEventListener("click", function () {
          run("stocktake-open", start, function () { return api("POST", "/api/stocktakes", { note: note.value.trim() }); }).then(function (v) { if (v) { toast(T("Stocktake started."), "ok"); setStocktake(v); loadStocktake(); } });
        });
        box.appendChild(h("div", { class: "card stack" }, h("h2", {}, T("Stocktake")),
          h("p", { class: "fine" }, T("Count what is on the shelves, ingredient by ingredient, over as long as it takes. Nothing changes in the stock until you apply the stocktake: then the differences are written, so what was sold in between is not taken off twice.")),
          h("div", { class: "row" }, note, start)));
      } else {
        var lines = {};
        (st.lines || []).forEach(function (l) { lines[l.ingredient] = l; });
        var applying = st.status === "applying";
        var rows = d.rows.map(function (r) {
          var l = lines[r.ingredient];
          var input = h("input", { type: "text", inputmode: "numeric", "aria-label": T("Counted"), placeholder: l ? "" : "–", disabled: applying || null });
          if (l) input.value = String(l.counted);
          var save = h("button", { type: "button", class: "small ghost", disabled: applying || null }, T("Save"));
          function send() {
            var n = p.intOf(input.value);
            if (n === null || n < 0) { toast(T("The quantity is a whole number, zero or more."), "bad"); return; }
            run("count:" + st.id + ":" + r.ingredient + ":" + n, save, function () { return api("PUT", "/api/stocktakes/" + st.id + "/lines", { ingredient: r.ingredient, counted: n }); }).then(function (v) { if (v) setStocktake(v); });
          }
          save.addEventListener("click", send);
          input.addEventListener("keydown", function (e) { if (e.key === "Enter") { e.preventDefault(); send(); } });
          var v = l ? p.variance(l) : null;
          return h("tr", {}, h("td", { "data-label": T("Ingredient") }, h("b", {}, nameOf(r.ingredient))),
            h("td", { class: "n", "data-label": T("In stock") }, qtyText(r.ingredient, r.qty)),
            h("td", { "data-label": T("Counted") }, h("div", { class: "row tight" }, input, save)),
            h("td", { class: "n", "data-label": T("Difference") }, l ? h("span", { class: v === 0 ? "" : v < 0 ? "problem" : "ok-text" }, (v > 0 ? "+" : "") + v) : ""),
            h("td", {}, l && !applying ? h("button", { type: "button", class: "small ghost", "aria-label": T("Remove"), title: T("Remove"), onclick: function () {
              api("DELETE", "/api/stocktakes/" + st.id + "/lines/" + encodeURIComponent(r.ingredient)).then(setStocktake, fail);
            } }, "×") : null));
        });
        var counted = (st.lines || []).length, diff = (st.lines || []).filter(function (l) { return p.variance(l) !== 0; }).length;
        var applyBtn = twoStep(h("button", { type: "button", class: "primary" }, applying ? T("Finish applying") : T("Apply the stocktake")), applying ? T("Finish applying") : T("Apply the stocktake"), function (btn) {
          btn.disabled = true;
          api("POST", "/api/stocktakes/" + st.id + "/apply", {}).then(function (r) {
            toast(T("Stocktake applied: {n} ingredient(s) changed.", { n: (r.applied || []).length }), "ok");
            W.stocktake = null; W.stocktakes = null; W.stocktake = undefined; load();
          }, function (e) { fail(e); load(); loadStocktake(); }).then(function () { btn.disabled = false; });
        });
        var cancel = !applying ? twoStep(h("button", { type: "button", class: "ghost" }, T("Throw it away")), T("Throw it away"), function (btn) {
          btn.disabled = true;
          api("POST", "/api/stocktakes/" + st.id + "/cancel", {}).then(function () { W.stocktake = undefined; W.stocktakes = null; W.redraw(); }, fail).then(function () { btn.disabled = false; });
        }) : null;
        box.appendChild(h("div", { class: "card stack" },
          h("div", { class: "row" }, h("h2", { class: "grow" }, T("Stocktake {n}", { n: st.id })), h("span", { class: "fine" }, T("opened {when} by {who}", { when: when(st.opened_at), who: st.opened_by }))),
          st.note ? h("p", {}, st.note) : null,
          applying ? h("div", { class: "banner warn", role: "status" }, T("This stocktake was being applied and was cut short. Press the button to finish: what was already written is not written twice.")) : null,
          h("div", { class: "scroll" }, h("table", { class: "stackrows" },
            h("thead", {}, h("tr", {}, h("th", {}, T("Ingredient")), h("th", { class: "n" }, T("In stock")), h("th", {}, T("Counted")), h("th", { class: "n" }, T("Difference")), h("th", {}, ""))), h("tbody", {}, rows))),
          h("p", { class: "fine" }, T("{counted} counted, {diff} with a difference. The difference is what was found against what the stock said when it was counted.", { counted: counted, diff: diff })),
          h("div", { class: "row" }, applyBtn, cancel)));
      }
      var past = (W.stocktakes || []).filter(function (s) { return !st || s.id !== st.id; });
      if (past.length) box.appendChild(h("div", { class: "card stack" }, h("h2", {}, T("Earlier stocktakes")), h("ul", { class: "rowlist" }, past.map(function (s) {
        return h("li", { class: "rowitem" }, h("div", { class: "grow" }, h("b", {}, T("Stocktake {n}", { n: s.id })), s.note ? h("small", { class: "fine" }, " " + s.note) : null),
          h("span", { class: "badge " + (s.status === "applied" ? "ready" : "cancelled") }, s.status === "applied" ? T("applied") : T("thrown away")),
          h("div", { class: "fine" }, when(s.closed_at || s.opened_at) + " · " + (s.closed_by || s.opened_by)));
      }))));
      return box;
    }

    // ---- suppliers and ingredients -----------------------------------------------------

    function suppliersTab() {
      var d = W.data, box = h("div", { class: "stack" });
      var mayEdit = can("warehouse.manage");
      var edit = W.supplierEdit;
      var list = h("div", { class: "card stack" }, h("div", { class: "row" }, h("h2", { class: "grow" }, T("Suppliers")),
        mayEdit ? h("button", { type: "button", class: "small primary", onclick: function () { W.supplierEdit = { isNew: true, id: "", name: "", phone: "", note: "", active: true }; W.redraw(); } }, T("Add a supplier")) : null));
      var sups = d.suppliers || [];
      if (!sups.length) list.appendChild(h("p", { class: "empty" }, T("No supplier yet.")));
      else list.appendChild(h("ul", { class: "rowlist" }, sups.map(function (s) {
        return h("li", { class: "rowitem" + (s.active ? "" : " off") }, h("div", { class: "grow" }, h("b", {}, s.name), h("small", { class: "fine" }, " " + s.id), s.phone || s.note ? h("div", { class: "fine" }, [s.phone, s.note].filter(Boolean).join(" · ")) : null),
          s.active ? null : h("span", { class: "badge soldout" }, T("not in use")),
          mayEdit ? h("button", { type: "button", class: "small ghost", onclick: function () { W.supplierEdit = { isNew: false, id: s.id, name: s.name, phone: s.phone || "", note: s.note || "", active: s.active }; W.redraw(); } }, T("Edit")) : null);
      })));
      box.appendChild(list);
      if (edit) {
        function bind(key, attrs) {
          var el = h("input", Object.assign({ type: "text", autocomplete: "off" }, attrs)); el.value = edit[key];
          el.addEventListener("input", function () { edit[key] = el.value; }); return el;
        }
        var problem = h("p", { class: "problem", role: "alert" });
        var active = h("input", { type: "checkbox" }); active.checked = edit.active; active.addEventListener("change", function () { edit.active = active.checked; });
        var idIn = bind("id", { maxlength: "32", autocapitalize: "none", id: "sp-id" }); idIn.disabled = !edit.isNew;
        var saveBtn = h("button", { type: "button", class: "primary" }, edit.isNew ? T("Add the supplier") : T("Save the supplier"));
        saveBtn.addEventListener("click", function () {
          if (!edit.name.trim()) { problem.textContent = T("Give the supplier a name."); return; }
          if (edit.isNew && !/^[a-z0-9][a-z0-9_-]{1,31}$/.test(edit.id)) { problem.textContent = T("An ID is 2 to 32 lower-case letters, digits, dashes and underscores."); return; }
          problem.textContent = "";
          var body = { name: edit.name, phone: edit.phone, note: edit.note };
          var req = edit.isNew ? api("POST", "/api/suppliers", Object.assign({ id: edit.id }, body)) : api("PUT", "/api/suppliers/" + encodeURIComponent(edit.id), Object.assign({ active: edit.active }, body));
          saveBtn.disabled = true;
          req.then(function () { toast(T("Saved."), "ok"); W.supplierEdit = null; return load(); }, function (e) { problem.textContent = errText(e); }).then(function () { saveBtn.disabled = false; });
        });
        box.appendChild(h("div", { class: "card stack" }, h("h2", {}, edit.isNew ? T("New supplier") : T("Edit supplier")),
          fieldOf("sp-id", T("Id"), idIn), fieldOf("sp-name", T("Name"), bind("name", { maxlength: "80", id: "sp-name" })),
          fieldOf("sp-phone", T("Phone"), bind("phone", { maxlength: "30", id: "sp-phone" })), fieldOf("sp-note", T("Note"), bind("note", { maxlength: "200", id: "sp-note" })),
          edit.isNew ? null : h("label", { class: "opt" }, active, T("In use")), problem,
          h("div", { class: "row" }, saveBtn, h("button", { type: "button", class: "ghost", onclick: function () { W.supplierEdit = null; W.redraw(); } }, T("Cancel")))));
      }
      // names and units of the ingredients
      var ing = h("div", { class: "card stack" }, h("h2", {}, T("Names and units of the ingredients")),
        h("p", { class: "fine" }, T("A name and a unit make the stock readable; the shelf life suggests a date when a delivery is received.")));
      ing.appendChild(h("div", { class: "scroll" }, h("table", { class: "stackrows" },
        h("thead", {}, h("tr", {}, h("th", {}, T("Ingredient")), h("th", {}, T("Name")), h("th", {}, T("Unit")), h("th", {}, T("Keeps (days)")), h("th", {}, ""))),
        h("tbody", {}, d.rows.map(function (r) {
          var i = (d.infos && d.infos[r.ingredient]) || {};
          var name = h("input", { type: "text", maxlength: "80", autocomplete: "off", "aria-label": T("Name"), disabled: !mayEdit || null }); name.value = i.name || "";
          var unit = h("input", { type: "text", maxlength: "12", autocomplete: "off", "aria-label": T("Unit"), placeholder: "g, ml, pc", disabled: !mayEdit || null }); unit.value = i.unit || "";
          var days = h("input", { type: "text", inputmode: "numeric", "aria-label": T("Keeps (days)"), disabled: !mayEdit || null }); days.value = i.shelf_days ? String(i.shelf_days) : "";
          var btn = h("button", { type: "button", class: "small ghost", disabled: !mayEdit || null }, T("Save"));
          btn.addEventListener("click", function () {
            var n = days.value.trim() === "" ? 0 : p.intOf(days.value);
            if (n === null || n < 0) { toast(T("A shelf life is a whole number of days, or empty."), "bad"); return; }
            if (!name.value.trim() || !unit.value.trim()) { toast(T("Give a name and a unit."), "bad"); return; }
            btn.disabled = true;
            api("PUT", "/api/ingredients/" + encodeURIComponent(r.ingredient), { name: name.value, unit: unit.value, shelf_days: n }).then(function () { toast(T("Saved."), "ok"); return load(); }, fail).then(function () { btn.disabled = false; });
          });
          return h("tr", {}, h("td", { "data-label": T("Ingredient") }, h("code", { class: "code" }, r.ingredient)), h("td", { "data-label": T("Name") }, name), h("td", { "data-label": T("Unit") }, unit), h("td", { "data-label": T("Keeps (days)") }, days), h("td", {}, mayEdit ? btn : null));
        })))));
      box.appendChild(ing);
      return box;
    }

    // ---- the screen --------------------------------------------------------------------

    function tabs() {
      var out = [["overview", T("Stock")]];
      if (can("stock.receive")) out.push(["receive", T("Receive")]);
      out.push(["history", T("History")], ["waste", T("Waste")]);
      if (can("stock.adjust") && hasDB()) out.push(["stocktake", T("Stocktake")]);
      if (hasDB()) out.push(["suppliers", T("Suppliers")]);
      return out;
    }

    function screen(root) {
      var nav = h("div", { class: "subnav", role: "tablist" }), body = h("div", { class: "stack" });
      function draw() {
        clear(nav); clear(body);
        if (!W.data) { body.appendChild(h("div", { class: "card" }, W.problem ? h("p", { class: "problem", role: "alert" }, W.problem) : h("p", { class: "empty" }, T("Loading the stock…")))); return; }
        var list = tabs();
        if (!list.some(function (x) { return x[0] === W.tab; })) W.tab = "overview";
        list.forEach(function (x) {
          nav.appendChild(h("button", { type: "button", role: "tab", "aria-selected": W.tab === x[0] ? "true" : "false", onclick: function () { W.tab = x[0]; if (x[0] === "history" && W.ledgerIng && !W.ledger) loadLedger(); W.redraw(); } }, x[1]));
        });
        var view = W.tab === "receive" ? receiveTab() : W.tab === "history" ? historyTab() : W.tab === "waste" ? wasteTab() : W.tab === "stocktake" ? stocktakeTab() : W.tab === "suppliers" ? suppliersTab() : overview();
        body.appendChild(view);
      }
      W.redraw = draw;
      root.appendChild(h("h1", {}, T("Stock")));
      root.appendChild(h("div", { class: "stack" }, nav, body));
      draw();
      load();
      var timer = null;
      return {
        update: function () { /* the forms keep what is typed; they are drawn when something is done or arrives */ },
        // told when the stock moved: look again, but not under a form that is being filled in
        reload: function () {
          clearTimeout(timer);
          timer = setTimeout(function () {
            var typing = document.activeElement && /^(INPUT|SELECT|TEXTAREA)$/.test(document.activeElement.tagName);
            api("GET", "/api/warehouse").then(function (d) { W.data = d; W.problem = ""; if (!typing) W.redraw(); }, function () { /* as it was */ });
          }, 400);
        },
      };
    }

    return { stock: screen };
  };

  Warehouse.pure = pure;
  if (typeof module === "object" && module.exports) module.exports = Warehouse;
  else window.KitchenWarehouse = Warehouse;
})();
