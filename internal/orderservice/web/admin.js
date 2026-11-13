// The screens of the managers: the menu (items, photos, option groups, prices by time, combos, the
// low-stock thresholds) and the promotions. Both edit a document that the service keeps in its
// database as numbered versions; a save makes the next version and takes effect at once, and an
// old version can be put back. Like app.js, everything shown is put on the page with textContent
// and element creation, never as HTML.
//
// A screen keeps what it is editing outside the screen itself (in the store of its document), so
// that changing the language, which draws the screen again, does not lose a half-written item.
(function () {
  "use strict";

  // pure holds what turns the drafts of the forms into documents and back. It does not touch the
  // page, so the tests run it (admin.test.js); T is the translation of the moment.
  function pure(T) {
    var DAYS = ["mon", "tue", "wed", "thu", "fri", "sat", "sun"];

    function clone(x) { return JSON.parse(JSON.stringify(x)); }
    function str(v) { return v === undefined || v === null ? "" : String(v); }
    // intOf reads a whole number; anything else (a fraction, letters, nothing) is null.
    function intOf(s) { s = str(s).trim(); return /^-?\d+$/.test(s) ? parseInt(s, 10) : null; }
    function bad(msg) { return { err: msg }; }

    function rowsOf(recipe) {
      return Object.keys(recipe || {}).sort().map(function (k) { return { ing: k, qty: String(recipe[k]) }; });
    }
    function recipeOf(rows) {
      var out = {}, n = 0;
      for (var i = 0; i < rows.length; i++) {
        var ing = rows[i].ing.trim(), qty = rows[i].qty;
        if (!ing && !str(qty).trim()) continue;
        var q = intOf(qty);
        if (!ing || q === null || q < 1) return bad(T("Each line of a recipe needs an ingredient and a whole quantity above zero."));
        if (Object.prototype.hasOwnProperty.call(out, ing)) return bad(T("An ingredient is used twice in one recipe: {name}.", { name: ing }));
        out[ing] = q; n++;
      }
      return { obj: out, n: n };
    }
    function draftOfItem(it) {
      return {
        id: it.id, name: it.name, category: it.category, station: it.station, price: String(it.price), active: it.active !== false,
        recipe: rowsOf(it.recipe),
        time_prices: (it.time_prices || []).map(function (tp) { return { days: (tp.days || []).slice(), from: tp.from, to: tp.to, price: String(tp.price) }; }),
        groups: (it.groups || []).map(function (g) {
          return { id: g.id, name: g.name, min: String(g.min), max: String(g.max), options: (g.options || []).map(function (o) {
            return { id: o.id, name: o.name, delta: String(o.delta || 0), recipe: rowsOf(o.recipe) };
          }) };
        }),
      };
    }
    function newItemDraft() {
      return { id: "", name: "", category: "", station: "", price: "", active: true, recipe: [], time_prices: [], groups: [] };
    }

    // itemFromDraft checks what can be seen here and builds the item of the document; the
    // service checks the rest.
    function itemFromDraft(d) {
      var it = { id: d.id.trim(), name: d.name.trim(), category: d.category.trim(), station: d.station.trim() };
      if (!it.id) return bad(T("Give the item an id."));
      if (!it.name) return bad(T("Give the item a name."));
      if (!it.category) return bad(T("Give the item a category."));
      if (!it.station) return bad(T("Give the item a station."));
      var price = intOf(d.price);
      if (price === null || price < 0) return bad(T("The price is a whole number of đồng, zero or more."));
      it.price = price;
      var rec = recipeOf(d.recipe);
      if (rec.err) return rec;
      if (rec.n) it.recipe = rec.obj;
      var tps = [];
      for (var i = 0; i < d.time_prices.length; i++) {
        var tp = d.time_prices[i], p = intOf(tp.price);
        if (!tp.from.trim() || !tp.to.trim() || p === null || p < 0) return bad(T("A price by time needs a start, an end like 17:00, and a price."));
        var entry = { from: tp.from.trim(), to: tp.to.trim(), price: p };
        var days = DAYS.filter(function (x) { return tp.days.indexOf(x) >= 0; });
        if (days.length) entry.days = days;
        tps.push(entry);
      }
      if (tps.length) it.time_prices = tps;
      var groups = [];
      for (var gi = 0; gi < d.groups.length; gi++) {
        var g = d.groups[gi], mn = intOf(g.min), mx = intOf(g.max);
        if (!g.id.trim() || !g.name.trim()) return bad(T("Each group of options needs an id and a name."));
        if (mn === null || mx === null || mn < 0 || mx < 0) return bad(T("The least and the most to choose in a group are whole numbers."));
        var opts = [];
        for (var oi = 0; oi < g.options.length; oi++) {
          var o = g.options[oi], dl = intOf(o.delta);
          if (!o.id.trim() || !o.name.trim()) return bad(T("Each option needs an id and a name."));
          if (dl === null) return bad(T("The extra price of an option is a whole number, zero or more or less."));
          var orec = recipeOf(o.recipe);
          if (orec.err) return orec;
          var op = { id: o.id.trim(), name: o.name.trim(), delta: dl };
          if (orec.n) op.recipe = orec.obj;
          opts.push(op);
        }
        groups.push({ id: g.id.trim(), name: g.name.trim(), min: mn, max: mx, options: opts });
      }
      if (groups.length) it.groups = groups;
      if (!d.active) it.active = false;
      return { item: it };
    }

    function draftOfCombo(c) {
      return { id: c.id, name: c.name, price: String(c.price), active: c.active !== false,
        slots: (c.slots || []).map(function (s) { return { name: s.name, items: (s.items || []).slice() }; }) };
    }
    function comboFromDraft(d) {
      var c = { id: d.id.trim(), name: d.name.trim() }, p = intOf(d.price);
      if (!c.id || !c.name) return bad(T("A combo needs an id and a name."));
      if (p === null || p < 0) return bad(T("The price is a whole number of đồng, zero or more."));
      c.price = p;
      c.slots = [];
      for (var i = 0; i < d.slots.length; i++) {
        var s = d.slots[i];
        if (!s.name.trim() || !s.items.length) return bad(T("Each choice of a combo needs a name and at least one item."));
        c.slots.push({ name: s.name.trim(), items: s.items.slice() });
      }
      if (!d.active) c.active = false;
      return { combo: c };
    }

    function toLocalInput(iso) {
      if (!iso) return "";
      var d = new Date(iso);
      if (isNaN(d.getTime())) return "";
      var p = function (n) { return String(n).padStart(2, "0"); };
      return d.getFullYear() + "-" + p(d.getMonth() + 1) + "-" + p(d.getDate()) + "T" + p(d.getHours()) + ":" + p(d.getMinutes());
    }
    // fromLocalInput reads the time a person typed as local time and gives the moment, in RFC 3339.
    function fromLocalInput(v) {
      if (!v) return "";
      var d = new Date(v);
      return isNaN(d.getTime()) ? null : d.toISOString();
    }

    function draftOfPromo(p) {
      var sc = p.scope || {};
      return { id: p.id, name: p.name, kind: p.kind, code: p.code || "",
        percent: p.percent_bp ? String(p.percent_bp / 100) : "", amount: p.amount ? String(p.amount) : "",
        buy: p.buy ? String(p.buy) : "", free: p.free ? String(p.free) : "",
        min_subtotal: p.min_subtotal ? String(p.min_subtotal) : "", max_discount: p.max_discount ? String(p.max_discount) : "",
        valid_from: toLocalInput(p.valid_from), valid_to: toLocalInput(p.valid_to),
        exclusive: !!p.exclusive, priority: p.priority ? String(p.priority) : "", single_use: !!p.single_use,
        categories: (sc.categories || []).slice(), items: (sc.items || []).slice() };
    }
    function newPromoDraft() {
      return draftOfPromo({ id: "", name: "", kind: "percent_off" });
    }
    function optInt(s, what) {
      if (!str(s).trim()) return { v: 0 };
      var n = intOf(s);
      return n === null || n < 0 ? bad(what) : { v: n };
    }
    function promoFromDraft(d) {
      var p = { id: d.id.trim(), name: d.name.trim(), kind: d.kind };
      if (!p.id || !p.name) return bad(T("A promotion needs an id and a name."));
      var whole = T("Amounts, counts and the priority are whole numbers, zero or more.");
      var a = optInt(d.amount, whole), b = optInt(d.buy, whole), f = optInt(d.free, whole), ms = optInt(d.min_subtotal, whole), md = optInt(d.max_discount, whole), pr = optInt(d.priority, whole);
      var errs = [a, b, f, ms, md, pr].filter(function (x) { return x.err; });
      if (errs.length) return errs[0];
      if (d.kind === "percent_off") {
        var pct = Number(str(d.percent).trim().replace(",", "."));
        var bp = Math.round(pct * 100);
        if (!str(d.percent).trim() || !isFinite(pct) || pct <= 0 || pct > 100 || Math.abs(pct * 100 - bp) > 1e-6) return bad(T("The percentage is above 0 and up to 100, with at most two decimals."));
        p.percent_bp = bp;
        if (md.v) p.max_discount = md.v;
      } else if (d.kind === "fixed_off") {
        if (!a.v) return bad(T("Give the amount taken off, in đồng."));
        p.amount = a.v;
      } else {
        if (!b.v || !f.v) return bad(T("Give how many to buy and how many are free."));
        p.buy = b.v; p.free = f.v;
      }
      if (d.code.trim()) p.code = d.code.trim();
      if (ms.v) p.min_subtotal = ms.v;
      var from = fromLocalInput(d.valid_from), to = fromLocalInput(d.valid_to);
      if (from === null || to === null) return bad(T("A date and time like 2026-12-01 08:00."));
      if (from) p.valid_from = from;
      if (to) p.valid_to = to;
      if (d.exclusive) p.exclusive = true;
      if (pr.v) p.priority = pr.v;
      if (d.single_use) p.single_use = true;
      if (d.categories.length || d.items.length) {
        p.scope = {};
        if (d.categories.length) p.scope.categories = d.categories.slice();
        if (d.items.length) p.scope.items = d.items.slice();
      }
      return { promo: p };
    }
    return { DAYS, clone, str, intOf, bad, rowsOf, recipeOf, draftOfItem, newItemDraft, itemFromDraft, draftOfCombo, comboFromDraft, toLocalInput, fromLocalInput, draftOfPromo, newPromoDraft, promoFromDraft };
  }

  var KitchenAdmin = function (ui) {
    var h = ui.h, clear = ui.clear, T = ui.T, api = ui.api, toast = ui.toast, fieldOf = ui.fieldOf;
    var twoStep = ui.twoStep, when = ui.when, money = ui.money;
    var PHOTO_MAX = 1 << 20;
    var p = pure(T);
    var DAYS = p.DAYS, clone = p.clone, str = p.str, intOf = p.intOf, bad = p.bad, rowsOf = p.rowsOf, recipeOf = p.recipeOf, draftOfItem = p.draftOfItem, newItemDraft = p.newItemDraft, itemFromDraft = p.itemFromDraft, draftOfCombo = p.draftOfCombo, comboFromDraft = p.comboFromDraft, toLocalInput = p.toLocalInput, fromLocalInput = p.fromLocalInput, draftOfPromo = p.draftOfPromo, newPromoDraft = p.newPromoDraft, promoFromDraft = p.promoFromDraft;

    function dayLabel(d) {
      switch (d) {
        case "mon": return T("Mon");
        case "tue": return T("Tue");
        case "wed": return T("Wed");
        case "thu": return T("Thu");
        case "fri": return T("Fri");
        case "sat": return T("Sat");
        case "sun": return T("Sun");
      }
      return d;
    }

    // ---- small things ---------------------------------------------------------------------

    // inp is a text box that writes what is typed into obj[key], so that the draft is always the
    // truth and a screen drawn again shows it.
    function inp(obj, key, attrs) {
      var a = { type: "text", autocomplete: "off", spellcheck: "false" };
      Object.keys(attrs || {}).forEach(function (k) { a[k] = attrs[k]; });
      var el = h("input", a);
      el.value = str(obj[key]);
      el.addEventListener("input", function () { obj[key] = el.value; });
      return el;
    }
    function check(obj, key, label) {
      var el = h("input", { type: "checkbox" });
      el.checked = !!obj[key];
      el.addEventListener("change", function () { obj[key] = el.checked; });
      return h("label", { class: "opt" }, el, label);
    }
    // picks is a row of boxes to tick, for a list of names kept in obj[key].
    function picks(obj, key, all, labelOf) {
      var box = h("div", { class: "opts" });
      all.forEach(function (v) {
        var el = h("input", { type: "checkbox" });
        el.checked = obj[key].indexOf(v) >= 0;
        el.addEventListener("change", function () {
          var at = obj[key].indexOf(v);
          if (el.checked && at < 0) obj[key].push(v);
          if (!el.checked && at >= 0) obj[key].splice(at, 1);
        });
        box.appendChild(h("label", { class: "opt" }, el, labelOf ? labelOf(v) : v));
      });
      return box;
    }
    function removeBtn(onclick) { return h("button", { type: "button", class: "small ghost", "aria-label": T("Remove"), title: T("Remove"), onclick: onclick }, "×"); }

    // The stores. One per document; see the top of the file.
    function newStore(path) {
      return { path: path, version: null, doc: null, versions: null, json: "", sel: null, shown: null, ingredients: [], images: {},
        newer: false, stale: false, problem: "", redraw: function () { /* the screen sets this */ } };
    }
    var menuStore = newStore("/api/admin/menu"), promoStore = newStore("/api/admin/promotions");

    function adopt(P, r) {
      P.version = r.version; P.doc = r.doc; P.low = null;
      P.ingredients = r.ingredients || P.ingredients || [];
      P.images = r.images || P.images || {};
      P.json = JSON.stringify(r.doc, null, 2);
    }
    function load(P) {
      return Promise.all([api("GET", P.path), api("GET", P.path + "/versions")]).then(function (rs) {
        adopt(P, rs[0]); P.versions = rs[1].versions; P.stale = false; P.newer = false; P.problem = "";
        P.redraw();
      }, function (err) { P.problem = err && err.message ? err.message : ""; P.redraw(); });
    }
    // look is the periodic check: another manager may have saved. What is on the screen is only
    // replaced when nothing is being edited, or the person says so.
    function look(P) {
      if (!P.version) return;
      api("GET", P.path).then(function (r) {
        if (r.version.version === P.version.version) { P.newer = false; return; }
        if (!P.sel && P.json === JSON.stringify(P.doc, null, 2)) { load(P); return; }
        P.newer = true; P.redraw();
      }, function () { /* the screen stays as it was */ });
    }
    // save puts a document in use: the next version, made on the one that was read.
    function save(P, btn, doc, note, after) {
      if (btn) btn.disabled = true;
      return api("PUT", P.path, { base_version: P.version.version, note: note, doc: doc }).then(function () {
        toast(T("Saved. It is in use now."), "ok");
        if (after) after();
        ui.loadMenu();
        return load(P);
      }, function (err) {
        if (err && err.body && err.body.code === "stale") P.stale = true;
        toast(err && err.message ? err.message : T("The request did not get an answer. Press again to retry."), "bad");
      }).then(function () { if (btn) btn.disabled = false; P.redraw(); });
    }

    // ---- pieces both screens use ----------------------------------------------------------

    function statusCard(P, what) {
      var v = P.version;
      var box = h("div", { class: "card stack" });
      if (P.problem) box.appendChild(h("p", { class: "problem", role: "alert" }, P.problem));
      if (!v) { box.appendChild(h("p", { class: "empty" }, T("Loading…"))); return box; }
      box.appendChild(h("div", { class: "row" },
        h("div", { class: "grow" }, h("b", {}, T("Version {n}", { n: v.version })), h("small", { class: "fine" }, " · " + T("saved {when} by {who}", { when: when(v.at), who: v.by }))),
        h("button", { type: "button", class: "small ghost", onclick: function () { load(P); } }, T("Reload"))));
      if (P.stale) box.appendChild(h("div", { class: "banner bad", role: "alert" }, T("Someone else changed {what} since you opened it. Reload to see their change, then make yours again.", { what: what })));
      else if (P.newer) box.appendChild(h("div", { class: "banner warn", role: "status" }, T("Someone else saved a newer version of {what}. Reload to see it; what you have open will be lost.", { what: what })));
      return box;
    }

    // historyCard lists the versions, shows one, and puts one back as a new version.
    function historyCard(P, what) {
      var card = h("details", { class: "card" }, h("summary", {}, T("History of {what}", { what: what })));
      if (!P.versions) return card;
      var current = P.version && P.version.version;
      card.appendChild(h("div", { class: "scroll" }, h("table", { class: "stackrows" },
        h("thead", {}, h("tr", {}, h("th", {}, T("Version")), h("th", {}, T("Saved")), h("th", {}, T("By")), h("th", {}, T("Note")), h("th", {}, ""))),
        h("tbody", {}, P.versions.map(function (v) {
          var acts = [h("button", { type: "button", class: "small ghost", onclick: function () {
            api("GET", P.path + "/versions/" + v.version).then(function (r) { P.shown = { n: v.version, text: JSON.stringify(r.doc, null, 2) }; P.redraw(); }, function (e) { toast(e.message, "bad"); });
          } }, T("Show"))];
          if (v.version !== current) {
            acts.push(twoStep(h("button", { type: "button", class: "small ghost" }, T("Put back")), T("Put back"), function (btn) {
              btn.disabled = true;
              api("POST", P.path + "/versions/" + v.version + "/restore", { base_version: current }).then(function () {
                toast(T("Version {n} is back in use, as a new version.", { n: v.version }), "ok");
                P.sel = null; ui.loadMenu(); return load(P);
              }, function (err) {
                if (err && err.body && err.body.code === "stale") P.stale = true;
                toast(err.message, "bad");
              }).then(function () { btn.disabled = false; P.redraw(); });
            }));
          }
          return h("tr", {},
            h("td", {}, h("b", {}, "#" + v.version), v.version === current ? h("small", { class: "fine" }, " " + T("in use")) : null),
            h("td", { "data-label": T("Saved") }, when(v.at)),
            h("td", { "data-label": T("By") }, v.by),
            h("td", { "data-label": T("Note") }, v.note),
            h("td", {}, h("div", { class: "acts" }, acts)));
        })))));
      if (P.shown) card.appendChild(h("div", { class: "stack" }, h("h2", {}, T("Version {n}", { n: P.shown.n })), h("pre", { class: "json" }, P.shown.text),
        h("button", { type: "button", class: "small ghost", onclick: function () { P.shown = null; P.redraw(); } }, T("Hide"))));
      if (P.openHistory) card.setAttribute("open", "");
      card.addEventListener("toggle", function () { P.openHistory = card.open; });
      return card;
    }

    // jsonCard is the whole document as text, for what the forms do not cover. It is checked by the
    // service with the same rules as a file.
    function jsonCard(P, what) {
      var card = h("details", { class: "card" }, h("summary", {}, T("Advanced: the {what} as text", { what: what })));
      var area = h("textarea", { class: "json", rows: "16", spellcheck: "false", "aria-label": T("The {what} as text", { what: what }) });
      area.value = P.json;
      area.addEventListener("input", function () { P.json = area.value; });
      var problem = h("p", { class: "problem", role: "alert" });
      var saveBtn = h("button", { type: "button", class: "primary", onclick: function () {
        var doc;
        try { doc = JSON.parse(P.json); } catch (e) { problem.textContent = T("This is not valid JSON: {error}", { error: e.message }); return; }
        problem.textContent = "";
        save(P, saveBtn, doc, "edited as text");
      } }, T("Save"));
      card.appendChild(h("div", { class: "stack" },
        h("p", { class: "fine" }, T("Every field of the document can be changed here. The service checks it as it checks a file, and says what is wrong without changing anything.")),
        area, problem,
        h("div", { class: "row" }, saveBtn, h("button", { type: "button", class: "ghost", onclick: function () { P.json = JSON.stringify(P.doc, null, 2); P.redraw(); } }, T("Back to the saved version")))));
      if (P.openJSON) card.setAttribute("open", "");
      card.addEventListener("toggle", function () { P.openJSON = card.open; });
      return card;
    }

    // ---- recipes ---------------------------------------------------------------------------

    function recipeEditor(rows) {
      var box = h("div", { class: "stack tight" });
      (function draw() {
        clear(box);
        rows.forEach(function (r, i) {
          box.appendChild(h("div", { class: "erow c3" },
            inp(r, "ing", { list: "ingredients", maxlength: "40", placeholder: T("Ingredient"), "aria-label": T("Ingredient"), autocapitalize: "none" }),
            inp(r, "qty", { inputmode: "numeric", placeholder: T("Quantity"), "aria-label": T("Quantity") }),
            removeBtn(function () { rows.splice(i, 1); draw(); })));
        });
        box.appendChild(h("div", {}, h("button", { type: "button", class: "small ghost", onclick: function () { rows.push({ ing: "", qty: "" }); draw(); } }, T("Add an ingredient"))));
      })();
      return box;
    }

    // ---- the menu -------------------------------------------------------------------------

    function photoUrl(P, id) {
      var hash = P.images[id];
      return hash ? "/api/images/" + encodeURIComponent(id) + "?v=" + hash.slice(0, 12) : "";
    }
    function thumb(P, it) {
      var url = photoUrl(P, it.id);
      return h("div", { class: "thumb" }, url ? h("img", { src: url, alt: "", loading: "lazy" }) : (it.name || "?").slice(0, 1).toUpperCase());
    }

    function screenMenu(root) {
      var P = menuStore;
      var datalist = h("datalist", { id: "ingredients" });
      var statusBox = h("div"), listBox = h("div", { class: "card stack" }), editBox = h("div", { class: "stack" });
      var comboBox = h("div", { class: "card stack" }), lowBox = h("div", { class: "card stack" }), historyBox = h("div"), jsonBox = h("div");
      var filter = h("input", { type: "search", class: "grow", placeholder: T("Search the items"), "aria-label": T("Search the items"), autocomplete: "off" });
      filter.value = P.q || "";
      filter.addEventListener("input", function () { P.q = filter.value; drawList(); });
      var low = P.low; // the rows of the thresholds being edited

      function items() { return (P.doc && P.doc.items) || []; }
      function combos() { return (P.doc && P.doc.combos) || []; }
      function stations() { var s = {}; items().forEach(function (i) { s[i.station] = 1; }); return Object.keys(s).sort(); }
      function categories() { var s = {}; items().forEach(function (i) { s[i.category] = 1; }); return Object.keys(s).sort(); }

      function withDoc(change) { var d = clone(P.doc); change(d); return d; }

      function setSel(sel) { P.sel = sel; drawEdit(); drawList(); editBox.scrollIntoView({ block: "nearest" }); }

      // ---- the list of items
      function drawList() {
        clear(listBox);
        listBox.appendChild(h("div", { class: "row" }, h("h2", { class: "grow" }, T("Items")),
          h("button", { type: "button", class: "primary small", onclick: function () { setSel({ kind: "item", isNew: true, draft: newItemDraft(), problem: "" }); } }, T("Add an item"))));
        listBox.appendChild(h("div", { class: "row" }, filter));
        if (!P.doc) return;
        var q = (P.q || "").trim().toLowerCase();
        var shown = items().filter(function (it) { return !q || it.name.toLowerCase().indexOf(q) >= 0 || it.id.toLowerCase().indexOf(q) >= 0 || it.category.toLowerCase().indexOf(q) >= 0; });
        if (!items().length) { listBox.appendChild(h("p", { class: "empty" }, T("The menu has no items yet."))); return; }
        if (!shown.length) { listBox.appendChild(h("p", { class: "empty" }, T("No item matches."))); return; }
        listBox.appendChild(h("ul", { class: "rowlist" }, shown.map(function (it) {
          var on = P.sel && P.sel.kind === "item" && !P.sel.isNew && P.sel.id === it.id;
          var toggle = h("button", { type: "button", class: "small ghost", onclick: function (e) {
            save(P, e.currentTarget, withDoc(function (d) {
              d.items.forEach(function (x) { if (x.id === it.id) { if (it.active === false) delete x.active; else x.active = false; } });
            }), (it.active === false ? "showed item " : "hid item ") + it.id);
          } }, it.active === false ? T("Show on the menu") : T("Hide"));
          return h("li", { class: "rowitem" + (on ? " on" : "") + (it.active === false ? " off" : "") },
            thumb(P, it),
            h("div", { class: "grow" }, h("b", {}, it.name), h("small", { class: "fine" }, " " + it.id),
              h("div", { class: "fine" }, it.category + " · " + it.station + ((it.time_prices || []).length ? " · " + T("price by time") : ""))),
            h("div", { class: "money" }, money(it.price)),
            it.active === false ? h("span", { class: "badge soldout" }, T("hidden")) : null,
            h("div", { class: "acts" },
              h("button", { type: "button", class: "small ghost", onclick: function () { setSel({ kind: "item", id: it.id, draft: draftOfItem(it), problem: "" }); } }, T("Edit")), toggle));
        })));
      }

      // ---- the editor of one item
      function groupEditor(d, redo) {
        var box = h("div", { class: "stack" });
        d.groups.forEach(function (g, gi) {
          var opts = h("div", { class: "stack" });
          g.options.forEach(function (o, oi) {
            opts.appendChild(h("div", { class: "sub stack" },
              h("div", { class: "erow c4" },
                inp(o, "id", { maxlength: "32", placeholder: T("Id"), "aria-label": T("Option id"), autocapitalize: "none" }),
                inp(o, "name", { maxlength: "60", placeholder: T("Name"), "aria-label": T("Option name"), spellcheck: "true" }),
                inp(o, "delta", { inputmode: "numeric", placeholder: T("Extra price"), "aria-label": T("Extra price") }),
                removeBtn(function () { g.options.splice(oi, 1); redo(); })),
              h("details", {}, h("summary", { class: "fine" }, T("Ingredients this option adds")), recipeEditor(o.recipe))));
          });
          box.appendChild(h("fieldset", { class: "card sub stack" },
            h("legend", {}, T("Group of options")),
            h("div", { class: "erow c3" },
              inp(g, "id", { maxlength: "32", placeholder: T("Id"), "aria-label": T("Group id"), autocapitalize: "none" }),
              inp(g, "name", { maxlength: "60", placeholder: T("Name"), "aria-label": T("Group name"), spellcheck: "true" }),
              removeBtn(function () { d.groups.splice(gi, 1); redo(); })),
            h("div", { class: "cols two-eq" },
              inp(g, "min", { inputmode: "numeric", placeholder: T("Least"), "aria-label": T("Least to choose") }),
              inp(g, "max", { inputmode: "numeric", placeholder: T("Most"), "aria-label": T("Most to choose") })),
            opts,
            h("div", {}, h("button", { type: "button", class: "small ghost", onclick: function () { g.options.push({ id: "", name: "", delta: "0", recipe: [] }); redo(); } }, T("Add an option")))));
        });
        box.appendChild(h("div", {}, h("button", { type: "button", class: "small ghost", onclick: function () { d.groups.push({ id: "", name: "", min: "0", max: "1", options: [] }); redo(); } }, T("Add a group of options"))));
        return box;
      }

      function timeEditor(d, redo) {
        var box = h("div", { class: "stack" });
        d.time_prices.forEach(function (tp, i) {
          box.appendChild(h("div", { class: "sub stack" },
            h("div", { class: "opts" }, DAYS.map(function (day) {
              var el = h("input", { type: "checkbox" });
              el.checked = tp.days.indexOf(day) >= 0;
              el.addEventListener("change", function () {
                var at = tp.days.indexOf(day);
                if (el.checked && at < 0) tp.days.push(day);
                if (!el.checked && at >= 0) tp.days.splice(at, 1);
              });
              return h("label", { class: "opt" }, el, dayLabel(day));
            })),
            h("div", { class: "erow c4" },
              inp(tp, "from", { maxlength: "5", placeholder: "15:00", "aria-label": T("From") }),
              inp(tp, "to", { maxlength: "5", placeholder: "17:00", "aria-label": T("Until") }),
              inp(tp, "price", { inputmode: "numeric", placeholder: T("Price"), "aria-label": T("Price") }),
              removeBtn(function () { d.time_prices.splice(i, 1); redo(); }))));
        });
        box.appendChild(h("p", { class: "fine" }, T("With no day ticked the price is for every day. The end time is not included.")));
        box.appendChild(h("div", {}, h("button", { type: "button", class: "small ghost", onclick: function () { d.time_prices.push({ days: [], from: "", to: "", price: "" }); redo(); } }, T("Add a price by time"))));
        return box;
      }

      function photoSection(sel) {
        if (sel.isNew) return h("p", { class: "fine" }, T("Save the item first; then it can have a photo."));
        var url = photoUrl(P, sel.id);
        var file = h("input", { type: "file", accept: "image/jpeg,image/png", "aria-label": T("Choose a photo") });
        file.addEventListener("change", function () {
          var f = file.files && file.files[0];
          if (!f) return;
          if (f.type !== "image/jpeg" && f.type !== "image/png") { toast(T("A photo is a JPEG or a PNG file."), "bad"); file.value = ""; return; }
          if (f.size > PHOTO_MAX) { toast(T("A photo is at most 1 MB."), "bad"); file.value = ""; return; }
          api("PUT", "/api/admin/menu/items/" + encodeURIComponent(sel.id) + "/image", f).then(function (r) {
            P.images = r.images || {}; toast(T("Photo saved."), "ok"); ui.loadMenu(); drawEdit(); drawList();
          }, function (err) { toast(err.message, "bad"); file.value = ""; });
        });
        var drop = url ? twoStep(h("button", { type: "button", class: "small ghost" }, T("Remove the photo")), T("Remove the photo"), function (btn) {
          btn.disabled = true;
          api("DELETE", "/api/admin/menu/items/" + encodeURIComponent(sel.id) + "/image").then(function (r) {
            P.images = r.images || {}; toast(T("Photo removed."), "ok"); ui.loadMenu(); drawEdit(); drawList();
          }, function (err) { toast(err.message, "bad"); }).then(function () { btn.disabled = false; });
        }) : null;
        return h("div", { class: "stack" },
          h("div", { class: "photo-now" }, url ? h("img", { src: url, alt: "" }) : h("p", { class: "fine" }, T("No photo yet: the card shows a drawing."))),
          h("div", { class: "row" }, file, drop),
          h("p", { class: "fine" }, T("JPEG or PNG, at most 1 MB, between 64 and 4096 pixels on each side. A photo counts at once; it does not make a new version of the menu.")));
      }

      function drawItemEditor() {
        var sel = P.sel, d = sel.draft;
        var problem = h("p", { class: "problem", role: "alert" }, sel.problem || "");
        var saveBtn = h("button", { type: "button", class: "primary", onclick: function () {
          var r = itemFromDraft(d);
          if (r.err) { sel.problem = r.err; problem.textContent = r.err; return; }
          sel.problem = ""; problem.textContent = "";
          if (sel.isNew && items().some(function (x) { return x.id === r.item.id; })) { sel.problem = T("There is an item with that id already."); problem.textContent = sel.problem; return; }
          var doc = withDoc(function (x) {
            x.items = x.items || [];
            if (sel.isNew) x.items.push(r.item);
            else x.items = x.items.map(function (y) { return y.id === sel.id ? r.item : y; });
          });
          save(P, saveBtn, doc, (sel.isNew ? "added item " : "edited item ") + r.item.id, function () {
            P.sel = { kind: "item", id: r.item.id, draft: draftOfItem(r.item), problem: "" };
          });
        } }, sel.isNew ? T("Add the item") : T("Save the item"));
        var idIn = inp(d, "id", { maxlength: "32", autocapitalize: "none" });
        idIn.disabled = !sel.isNew;
        var catList = h("datalist", { id: "categories" }, categories().map(function (c) { return h("option", { value: c }); }));
        var stList = h("datalist", { id: "stations" }, stations().map(function (c) { return h("option", { value: c }); }));
        function redo() { drawEdit(); }
        var del = !sel.isNew ? twoStep(h("button", { type: "button", class: "ghost" }, T("Delete the item")), T("Delete the item"), function (btn) {
          save(P, btn, withDoc(function (x) { x.items = x.items.filter(function (y) { return y.id !== sel.id; }); }), "deleted item " + sel.id, function () { P.sel = null; });
        }) : null;
        editBox.appendChild(h("div", { class: "card stack" },
          h("div", { class: "row" }, h("h2", { class: "grow" }, sel.isNew ? T("New item") : T("Edit {name}", { name: sel.id })),
            h("button", { type: "button", class: "small ghost", onclick: function () { setSel(null); } }, T("Close"))),
          catList, stList,
          fieldOf("it-id", T("Id"), Object.assign(idIn, { id: "it-id" })),
          fieldOf("it-name", T("Name"), inp(d, "name", { id: "it-name", maxlength: "60", spellcheck: "true" })),
          h("div", { class: "cols two-eq" },
            fieldOf("it-cat", T("Category"), inp(d, "category", { id: "it-cat", maxlength: "40", list: "categories" })),
            fieldOf("it-st", T("Station"), inp(d, "station", { id: "it-st", maxlength: "20", list: "stations", autocapitalize: "none" }))),
          fieldOf("it-price", T("Price (đồng)"), inp(d, "price", { id: "it-price", inputmode: "numeric" })),
          check(d, "active", T("For sale")),
          h("h2", {}, T("Ingredients of one")), recipeEditor(d.recipe),
          h("h2", {}, T("Groups of options")), groupEditor(d, redo),
          h("h2", {}, T("Price by time of day")), timeEditor(d, redo),
          problem,
          h("div", { class: "row" }, saveBtn, del)));
        editBox.appendChild(h("div", { class: "card stack" }, h("h2", {}, T("Photo")), photoSection(sel)));
      }

      // ---- combos
      function drawComboEditor() {
        var sel = P.sel, d = sel.draft;
        var problem = h("p", { class: "problem", role: "alert" }, sel.problem || "");
        var ids = items().map(function (i) { return i.id; });
        var nameOf = function (id) { var it = items().filter(function (i) { return i.id === id; })[0]; return it ? it.name : id; };
        var slots = h("div", { class: "stack" });
        d.slots.forEach(function (s, i) {
          slots.appendChild(h("div", { class: "sub stack" },
            h("div", { class: "erow c2" }, inp(s, "name", { maxlength: "60", placeholder: T("Name of the choice"), "aria-label": T("Name of the choice"), spellcheck: "true" }),
              removeBtn(function () { d.slots.splice(i, 1); drawEdit(); })),
            picks(s, "items", ids, nameOf)));
        });
        var saveBtn = h("button", { type: "button", class: "primary", onclick: function () {
          var r = comboFromDraft(d);
          if (r.err) { sel.problem = r.err; problem.textContent = r.err; return; }
          if (sel.isNew && combos().some(function (x) { return x.id === r.combo.id; })) { sel.problem = T("There is a combo with that id already."); problem.textContent = sel.problem; return; }
          var doc = withDoc(function (x) {
            x.combos = x.combos || [];
            if (sel.isNew) x.combos.push(r.combo); else x.combos = x.combos.map(function (y) { return y.id === sel.id ? r.combo : y; });
          });
          save(P, saveBtn, doc, (sel.isNew ? "added combo " : "edited combo ") + r.combo.id, function () {
            P.sel = { kind: "combo", id: r.combo.id, draft: draftOfCombo(r.combo), problem: "" };
          });
        } }, sel.isNew ? T("Add the combo") : T("Save the combo"));
        var idIn = inp(d, "id", { id: "cb-id", maxlength: "32", autocapitalize: "none" });
        idIn.disabled = !sel.isNew;
        var del = !sel.isNew ? twoStep(h("button", { type: "button", class: "ghost" }, T("Delete the combo")), T("Delete the combo"), function (btn) {
          save(P, btn, withDoc(function (x) { x.combos = x.combos.filter(function (y) { return y.id !== sel.id; }); }), "deleted combo " + sel.id, function () { P.sel = null; });
        }) : null;
        editBox.appendChild(h("div", { class: "card stack" },
          h("div", { class: "row" }, h("h2", { class: "grow" }, sel.isNew ? T("New combo") : T("Edit {name}", { name: sel.id })),
            h("button", { type: "button", class: "small ghost", onclick: function () { setSel(null); } }, T("Close"))),
          fieldOf("cb-id", T("Id"), idIn),
          fieldOf("cb-name", T("Name"), inp(d, "name", { id: "cb-name", maxlength: "60", spellcheck: "true" })),
          fieldOf("cb-price", T("Price (đồng)"), inp(d, "price", { id: "cb-price", inputmode: "numeric" })),
          check(d, "active", T("For sale")),
          h("h2", {}, T("What the customer chooses")), slots,
          h("div", {}, h("button", { type: "button", class: "small ghost", onclick: function () { d.slots.push({ name: "", items: [] }); drawEdit(); } }, T("Add a choice"))),
          problem,
          h("div", { class: "row" }, saveBtn, del)));
      }

      function drawEdit() {
        clear(editBox);
        if (!P.sel) { editBox.appendChild(h("div", { class: "card" }, h("p", { class: "empty" }, T("Choose an item to edit, or add one.")))); return; }
        if (P.sel.kind === "combo") drawComboEditor(); else drawItemEditor();
      }

      function drawCombos() {
        clear(comboBox);
        comboBox.appendChild(h("div", { class: "row" }, h("h2", { class: "grow" }, T("Combos")),
          h("button", { type: "button", class: "small ghost", onclick: function () { setSel({ kind: "combo", isNew: true, draft: { id: "", name: "", price: "", active: true, slots: [] }, problem: "" }); } }, T("Add a combo"))));
        if (!P.doc) return;
        if (!combos().length) { comboBox.appendChild(h("p", { class: "empty" }, T("No combos."))); return; }
        comboBox.appendChild(h("ul", { class: "rowlist" }, combos().map(function (c) {
          return h("li", { class: "rowitem" + (c.active === false ? " off" : "") },
            h("div", { class: "grow" }, h("b", {}, c.name), h("small", { class: "fine" }, " " + c.id)),
            h("div", { class: "money" }, money(c.price)),
            c.active === false ? h("span", { class: "badge soldout" }, T("hidden")) : null,
            h("button", { type: "button", class: "small ghost", onclick: function () { setSel({ kind: "combo", id: c.id, draft: draftOfCombo(c), problem: "" }); } }, T("Edit")));
        })));
      }

      // ---- the low-stock thresholds
      function drawLow() {
        clear(lowBox);
        lowBox.appendChild(h("h2", {}, T("When an ingredient counts as running low")));
        if (!P.doc) return;
        if (!P.low) P.low = rowsOf(P.doc.low_at);
        low = P.low;
        var problem = h("p", { class: "problem", role: "alert" });
        var rows = h("div", { class: "stack tight" });
        (function draw() {
          clear(rows);
          low.forEach(function (r, i) {
            rows.appendChild(h("div", { class: "erow c3" },
              inp(r, "ing", { list: "ingredients", maxlength: "40", placeholder: T("Ingredient"), "aria-label": T("Ingredient"), autocapitalize: "none" }),
              inp(r, "qty", { inputmode: "numeric", placeholder: T("At or below"), "aria-label": T("At or below") }),
              removeBtn(function () { low.splice(i, 1); draw(); })));
          });
          rows.appendChild(h("div", {}, h("button", { type: "button", class: "small ghost", onclick: function () { low.push({ ing: "", qty: "" }); draw(); } }, T("Add an ingredient"))));
        })();
        var btn = h("button", { type: "button", onclick: function () {
          var r = recipeOf(low);
          if (r.err) { problem.textContent = r.err; return; }
          problem.textContent = "";
          save(P, btn, withDoc(function (x) { if (r.n) x.low_at = r.obj; else delete x.low_at; }), "edited the low-stock thresholds", function () { P.low = null; });
        } }, T("Save the thresholds"));
        lowBox.appendChild(rows); lowBox.appendChild(problem); lowBox.appendChild(h("div", { class: "row" }, btn));
        lowBox.appendChild(h("p", { class: "fine" }, T("An ingredient with no line here never shows as running low.")));
      }

      function drawAll() {
        clear(datalist);
        (P.ingredients || []).forEach(function (i) { datalist.appendChild(h("option", { value: i })); });
        clear(statusBox).appendChild(statusCard(P, T("the menu")));
        drawList(); drawEdit(); drawCombos(); drawLow();
        clear(historyBox).appendChild(historyCard(P, T("the menu")));
        clear(jsonBox).appendChild(jsonCard(P, T("menu")));
      }
      P.redraw = drawAll;

      root.appendChild(h("h1", {}, T("Menu")));
      root.appendChild(h("div", { class: "stack" }, datalist, statusBox,
        h("div", { class: "cols two" }, h("div", { class: "stack" }, listBox, comboBox, lowBox), editBox),
        historyBox, jsonBox));
      drawAll();
      if (!P.doc) load(P); else look(P);
      return { update: function () { /* the editors keep what is typed; nothing is drawn again here */ }, reload: function () { look(P); } };
    }

    // ---- the promotions -------------------------------------------------------------------

    function promoSummary(p) {
      var s;
      if (p.kind === "percent_off") s = T("{p}% off", { p: String((p.percent_bp || 0) / 100) });
      else if (p.kind === "fixed_off") s = T("{amount} off", { amount: money(p.amount || 0) });
      else s = T("Buy {buy}, get {free} free", { buy: p.buy || 0, free: p.free || 0 });
      return s;
    }

    function screenPromotions(root) {
      var P = promoStore;
      var statusBox = h("div"), listBox = h("div", { class: "card stack" }), editBox = h("div", { class: "stack" }), historyBox = h("div"), jsonBox = h("div");
      function list() { return (P.doc || []); }
      function withDoc(change) { var d = clone(P.doc); change(d); return d; }
      function setSel(sel) { P.sel = sel; drawEdit(); drawList(); editBox.scrollIntoView({ block: "nearest" }); }
      function menuItems() { return (ui.S.menu && ui.S.menu.items) || []; }

      function drawList() {
        clear(listBox);
        listBox.appendChild(h("div", { class: "row" }, h("h2", { class: "grow" }, T("Promotions")),
          h("button", { type: "button", class: "primary small", onclick: function () { setSel({ isNew: true, draft: newPromoDraft(), problem: "" }); } }, T("Add a promotion"))));
        if (!P.doc) return;
        if (!list().length) { listBox.appendChild(h("p", { class: "empty" }, T("No promotions."))); return; }
        listBox.appendChild(h("ul", { class: "rowlist" }, list().map(function (p) {
          var on = P.sel && !P.sel.isNew && P.sel.id === p.id;
          var span = [];
          if (p.valid_from) span.push(T("from {date}", { date: when(p.valid_from) }));
          if (p.valid_to) span.push(T("until {date}", { date: when(p.valid_to) }));
          return h("li", { class: "rowitem" + (on ? " on" : "") },
            h("div", { class: "grow" }, h("b", {}, p.name), h("small", { class: "fine" }, " " + p.id),
              h("div", { class: "fine" }, [promoSummary(p)].concat(span).join(" · "))),
            p.code ? h("code", { class: "code" }, p.code) : h("span", { class: "fine" }, T("automatic")),
            h("button", { type: "button", class: "small ghost", onclick: function () { setSel({ id: p.id, draft: draftOfPromo(p), problem: "" }); } }, T("Edit")));
        })));
      }

      function drawEdit() {
        clear(editBox);
        var sel = P.sel;
        if (!sel) { editBox.appendChild(h("div", { class: "card" }, h("p", { class: "empty" }, T("Choose a promotion to edit, or add one.")))); return; }
        var d = sel.draft;
        var problem = h("p", { class: "problem", role: "alert" }, sel.problem || "");
        var kindSel = h("select", { id: "pr-kind", "aria-label": T("Kind") },
          [["percent_off", T("Percent off")], ["fixed_off", T("Amount off")], ["buy_x_get_y", T("Buy some, get some free")]].map(function (k) {
            return h("option", { value: k[0], selected: d.kind === k[0] || null }, k[1]);
          }));
        kindSel.addEventListener("change", function () { d.kind = kindSel.value; drawEdit(); });
        var idIn = inp(d, "id", { id: "pr-id", maxlength: "32", autocapitalize: "none" });
        idIn.disabled = !sel.isNew;
        var kindFields = d.kind === "percent_off" ? [
          fieldOf("pr-pct", T("Percent off"), inp(d, "percent", { id: "pr-pct", inputmode: "decimal", placeholder: "10" })),
          fieldOf("pr-max", T("Most taken off (đồng, empty for no limit)"), inp(d, "max_discount", { id: "pr-max", inputmode: "numeric" })),
        ] : d.kind === "fixed_off" ? [
          fieldOf("pr-amt", T("Amount off (đồng)"), inp(d, "amount", { id: "pr-amt", inputmode: "numeric" })),
        ] : [
          h("div", { class: "cols two-eq" },
            fieldOf("pr-buy", T("Buy"), inp(d, "buy", { id: "pr-buy", inputmode: "numeric" })),
            fieldOf("pr-free", T("Free"), inp(d, "free", { id: "pr-free", inputmode: "numeric" }))),
        ];
        var cats = {};
        menuItems().forEach(function (i) { cats[i.category] = 1; });
        var saveBtn = h("button", { type: "button", class: "primary", onclick: function () {
          var r = promoFromDraft(d);
          if (r.err) { sel.problem = r.err; problem.textContent = r.err; return; }
          if (sel.isNew && list().some(function (x) { return x.id === r.promo.id; })) { sel.problem = T("There is a promotion with that id already."); problem.textContent = sel.problem; return; }
          var doc = withDoc(function (x) {
            if (sel.isNew) x.push(r.promo); else x.forEach(function (y, i) { if (y.id === sel.id) x[i] = r.promo; });
          });
          save(P, saveBtn, doc, (sel.isNew ? "added promotion " : "edited promotion ") + r.promo.id, function () {
            P.sel = { id: r.promo.id, draft: draftOfPromo(r.promo), problem: "" };
          });
        } }, sel.isNew ? T("Add the promotion") : T("Save the promotion"));
        var del = !sel.isNew ? twoStep(h("button", { type: "button", class: "ghost" }, T("Delete the promotion")), T("Delete the promotion"), function (btn) {
          save(P, btn, withDoc(function (x) { var keep = x.filter(function (y) { return y.id !== sel.id; }); x.length = 0; keep.forEach(function (y) { x.push(y); }); }), "deleted promotion " + sel.id, function () { P.sel = null; });
        }) : null;
        editBox.appendChild(h("div", { class: "card stack" },
          h("div", { class: "row" }, h("h2", { class: "grow" }, sel.isNew ? T("New promotion") : T("Edit {name}", { name: sel.id })),
            h("button", { type: "button", class: "small ghost", onclick: function () { setSel(null); } }, T("Close"))),
          fieldOf("pr-id", T("Id"), idIn),
          fieldOf("pr-name", T("Name"), inp(d, "name", { id: "pr-name", maxlength: "60", spellcheck: "true" })),
          fieldOf("pr-kind", T("Kind"), kindSel),
          kindFields,
          fieldOf("pr-code", T("Code (empty for an offer that applies by itself)"), inp(d, "code", { id: "pr-code", maxlength: "32", autocapitalize: "characters" })),
          fieldOf("pr-min", T("Least subtotal (đồng, empty for none)"), inp(d, "min_subtotal", { id: "pr-min", inputmode: "numeric" })),
          h("div", { class: "cols two-eq" },
            fieldOf("pr-from", T("Valid from"), inp(d, "valid_from", { id: "pr-from", type: "datetime-local" })),
            fieldOf("pr-to", T("Valid until"), inp(d, "valid_to", { id: "pr-to", type: "datetime-local" }))),
          h("fieldset", {}, h("legend", {}, T("Only for these categories (none ticked: everything)")), picks(d, "categories", Object.keys(cats).sort())),
          h("fieldset", {}, h("legend", {}, T("Only for these items")), picks(d, "items", menuItems().map(function (i) { return i.id; }), function (id) {
            var it = menuItems().filter(function (i) { return i.id === id; })[0]; return it ? it.name : id;
          })),
          h("div", { class: "opts" }, check(d, "exclusive", T("Cannot be combined with another")), check(d, "single_use", T("Once for each customer"))),
          fieldOf("pr-prio", T("Priority (the higher is applied first)"), inp(d, "priority", { id: "pr-prio", inputmode: "numeric" })),
          problem,
          h("div", { class: "row" }, saveBtn, del)));
      }

      function drawAll() {
        clear(statusBox).appendChild(statusCard(P, T("the promotions")));
        drawList(); drawEdit();
        clear(historyBox).appendChild(historyCard(P, T("the promotions")));
        clear(jsonBox).appendChild(jsonCard(P, T("promotions")));
      }
      P.redraw = drawAll;
      root.appendChild(h("h1", {}, T("Promotions")));
      root.appendChild(h("div", { class: "stack" }, statusBox, h("div", { class: "cols two" }, listBox, editBox), historyBox, jsonBox));
      drawAll();
      if (!P.doc) load(P); else look(P);
      return { update: function () { /* see screenMenu */ }, reload: function () { look(P); } };
    }

    return { menu: screenMenu, promotions: screenPromotions };
  };
  KitchenAdmin.pure = pure;
  if (typeof module === "object" && module.exports) module.exports = KitchenAdmin;
  else window.KitchenAdmin = KitchenAdmin;
})();
