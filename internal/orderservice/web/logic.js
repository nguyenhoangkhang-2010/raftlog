// The parts of the screens that are plain functions: money, prices, the choices of an item,
// the way orders are grouped for each screen, and the parsing of the stream of changes. They
// touch no page, so they run in the browser and, for the tests, in Node.
(function (root, factory) {
  if (typeof module === "object" && module.exports) module.exports = factory();
  else root.KitchenLogic = factory();
})(typeof self !== "undefined" ? self : this, function () {
  "use strict";
  // The sentences are English, and the language of the moment decides what a person reads (i18n.js).
  var I18N = typeof KitchenI18n !== "undefined" ? KitchenI18n : require("./i18n.js");
  var T = I18N.t;

  // formatMoney writes a whole number of dong like the service does: 89100 is "89.100đ".
  function formatMoney(n) {
    var neg = n < 0;
    var digits = String(Math.abs(Math.trunc(n)));
    var out = "";
    for (var i = 0; i < digits.length; i++) {
      if (i > 0 && (digits.length - i) % 3 === 0) out += ".";
      out += digits[i];
    }
    return (neg ? "-" : "") + out + "đ";
  }

  // chosen returns the option IDs picked for a group in a choice ({groupID: [optionID]}).
  function chosen(choice, group) {
    return (choice && choice[group.id]) || [];
  }

  // groupProblem says what is wrong with the picks in one group, or "" if nothing is.
  function groupProblem(group, choice) {
    var n = chosen(choice, group).length;
    if (n < group.min) {
      return group.min === 1 && group.max === 1 ? T("Choose one: {name}", { name: group.name }) : T("Choose at least {n}: {name}", { n: group.min, name: group.name });
    }
    if (n > group.max) return T("Choose at most {n}: {name}", { n: group.max, name: group.name });
    return "";
  }

  // itemProblems lists what is wrong with the choice made for an item.
  function itemProblems(item, choice) {
    var out = [];
    (item.groups || []).forEach(function (g) {
      var p = groupProblem(g, choice);
      if (p) out.push(p);
    });
    return out;
  }

  // unitPrice is the price of one unit of the item with the choice, as the menu shows it. The
  // service prices the order; this is the figure shown while the cart is made.
  function unitPrice(item, choice) {
    var price = item.price;
    (item.groups || []).forEach(function (g) {
      var picked = chosen(choice, g);
      g.options.forEach(function (o) {
        if (picked.indexOf(o.id) >= 0) price += o.delta;
      });
    });
    return price;
  }

  // optionNames lists the names of the options picked for an item, in the order of the menu.
  function optionNames(item, choice) {
    var names = [];
    (item.groups || []).forEach(function (g) {
      var picked = chosen(choice, g);
      g.options.forEach(function (o) {
        if (picked.indexOf(o.id) >= 0) names.push(o.name);
      });
    });
    return names;
  }

  // toggleOption returns the choice after the option was clicked in its group: a group that takes
  // one option replaces it, a group that takes several adds or removes it. The choice is not changed.
  function toggleOption(choice, group, optionID) {
    var next = {};
    Object.keys(choice || {}).forEach(function (k) { next[k] = choice[k].slice(); });
    var cur = next[group.id] || [];
    var at = cur.indexOf(optionID);
    if (group.max === 1) {
      cur = at >= 0 && group.min === 0 ? [] : [optionID];
    } else if (at >= 0) {
      cur.splice(at, 1);
    } else if (cur.length < group.max) {
      cur.push(optionID);
    }
    if (cur.length) next[group.id] = cur; else delete next[group.id];
    return next;
  }

  // cartLine is what the cart holds for one selection. For an item: {item, options, quantity,
  // note}; for a combo: {combo, picks: [{slot, item, options}], quantity, note}.
  function cartEstimate(menu, lines) {
    var total = 0;
    lines.forEach(function (l) {
      var unit = 0;
      if (l.item) {
        var it = findItem(menu, l.item);
        if (it) unit = unitPrice(it, l.options);
      } else if (l.combo) {
        var c = (menu.combos || []).filter(function (x) { return x.id === l.combo; })[0];
        if (c) {
          unit = c.price;
          (l.picks || []).forEach(function (p) {
            var pit = findItem(menu, p.item);
            if (pit) unit += unitPrice(pit, p.options) - pit.price; // only the price of the options
          });
        }
      }
      total += unit * l.quantity;
    });
    return total;
  }

  function findItem(menu, id) {
    return (menu.items || []).filter(function (i) { return i.id === id; })[0];
  }

  // cartBody is the request that places the order.
  function cartBody(commandID, lines, codes, customer) {
    var body = { command_id: commandID, items: [] };
    lines.forEach(function (l) {
      var it = { quantity: l.quantity };
      if (l.note) it.note = l.note;
      if (l.item) {
        it.item = l.item;
        if (l.options && Object.keys(l.options).length) it.options = l.options;
      } else {
        it.combo = l.combo;
        it.picks = (l.picks || []).map(function (p) {
          var pick = { slot: p.slot, item: p.item };
          if (p.options && Object.keys(p.options).length) pick.options = p.options;
          return pick;
        });
      }
      body.items.push(it);
    });
    if (codes && codes.length) body.codes = codes;
    if (customer) body.customer = customer;
    return body;
  }

  // describeError turns the service's answer into a sentence for a person. The fixed messages of
  // the service are in the dictionary and are shown in the person's language; one that is not
  // there is shown in English, with a short sentence in the person's language before it.
  function describeError(status, body) {
    body = body || {};
    var msg = body.error ? T(body.error) : T("Something went wrong ({status})", { status: status });
    var untranslated = !!body.error && !I18N.has(body.error);
    switch (body.code) {
      case "out_of_stock":
        if (body.shortages && body.shortages.length) {
          msg = T("Not enough in stock: {list}", { list: body.shortages.map(function (s) {
            return T("{ingredient} (needs {need}, has {have})", { ingredient: s.ingredient, need: s.need, have: s.have });
          }).join(", ") });
        }
        break;
      case "cart":
        if (body.lines && body.lines.length) {
          msg = body.lines.map(function (l) {
            var why = l.error.replace(/^menu: /, "");
            var sold = /^sold out: (.*)$/.exec(why);
            return T("Line {n}: {error}", { n: l.line, error: sold ? T("sold out: {name}", { name: sold[1] }) : why });
          }).join("; ");
        }
        break;
      case "invalid_document": msg = T("Not saved: the service checked the document and says:") + " " + (body.error || ""); break;
      case "no_shift": msg = T("Open a shift before taking cash."); break;
      case "unavailable": msg = T("The kitchen system could not reach its log. Try again in a moment."); break;
      case "unauthorized": msg = T("The token was not accepted."); break;
      case "bad_credentials": msg = T("Wrong login name or password."); break;
      case "wrong_password": msg = T("The current password is wrong."); break;
      case "rate_limited": msg = T("Too many attempts from this address. Wait a moment and try again."); break;
      case "password_change_required": msg = T("Change your temporary password first."); break;
      case "cross_site": msg = T("This request was made by another site."); break;
      case "locked":
        if (body.retry_after) msg = T("Too many wrong passwords. Try again in {n} minutes, or ask an administrator to unlock the account.", { n: Math.max(1, Math.ceil(body.retry_after / 60)) });
        break;
      case "forbidden": if (untranslated) msg = T("You may not do that.") + " (" + body.error + ")"; break;
      case "not_found": if (untranslated) msg = T("That was not found.") + " (" + body.error + ")"; break;
      case "conflict": if (untranslated) msg = T("That cannot be done now.") + " (" + body.error + ")"; break;
      case "invalid": if (untranslated) msg = T("That is not valid.") + " (" + body.error + ")"; break;
      case "weak_password": if (untranslated) msg = T("That password is not accepted.") + " (" + body.error + ")"; break;
    }
    return msg;
  }

  var ACTIVE = ["placed", "paid", "preparing", "ready"];

  // groupForTill sorts orders into the three columns of the till.
  function groupForTill(orders) {
    var cols = { toPay: [], inKitchen: [], ready: [] };
    orders.forEach(function (o) {
      if (o.status === "placed") cols.toPay.push(o);
      else if (o.status === "paid" || o.status === "preparing") cols.inKitchen.push(o);
      else if (o.status === "ready") cols.ready.push(o);
    });
    Object.keys(cols).forEach(function (k) {
      cols[k].sort(function (a, b) { return a.number - b.number; });
    });
    return cols;
  }

  // stationWork returns the tickets of a station that are waiting or being made, oldest first. A
  // ticket has the order, the pieces that this station makes (with the quantity), the notes of the
  // lines that have pieces here, and whether a cancellation is waiting.
  function stationWork(orders, station) {
    var work = { waiting: [], cooking: [] };
    orders.forEach(function (o) {
      if (o.status !== "paid" && o.status !== "preparing") return;
      var t = (o.tickets || []).filter(function (x) { return x.station === station; })[0];
      if (!t || t.status === "ready") return;
      var pieces = [], notes = [];
      (o.lines || []).forEach(function (l) {
        var here = (l.pieces || []).filter(function (p) { return p.station === station; });
        here.forEach(function (p) { pieces.push({ name: p.name, quantity: l.quantity }); });
        if (here.length && l.note) notes.push(l.note);
      });
      var ticket = {
        order: o, ticket: t, pieces: mergePieces(pieces), notes: notes,
        since: t.status === "started" && t.started_at ? t.started_at : (o.payment && o.payment.at) || o.placed_at,
        cancelling: !!o.cancel_request,
      };
      (t.status === "started" ? work.cooking : work.waiting).push(ticket);
    });
    [work.waiting, work.cooking].forEach(function (l) {
      l.sort(function (a, b) { return Date.parse(a.since) - Date.parse(b.since); });
    });
    return work;
  }

  // mergePieces adds the quantities of pieces that have the same name.
  function mergePieces(pieces) {
    var by = {}, order = [];
    pieces.forEach(function (p) {
      if (!by[p.name]) { by[p.name] = { name: p.name, quantity: 0 }; order.push(p.name); }
      by[p.name].quantity += p.quantity;
    });
    return order.map(function (n) { return by[n]; });
  }

  // stationsOf lists the stations that appear in the orders, sorted.
  function stationsOf(orders) {
    var seen = {};
    orders.forEach(function (o) { (o.tickets || []).forEach(function (t) { seen[t.station] = true; }); });
    return Object.keys(seen).sort();
  }

  // cancelQueue is the orders with a cancellation request, oldest request first.
  function cancelQueue(orders) {
    return orders.filter(function (o) { return o.cancel_request; })
      .sort(function (a, b) { return Date.parse(a.cancel_request.at) - Date.parse(b.cancel_request.at); });
  }

  function workStarted(order) {
    return (order.tickets || []).some(function (t) { return t.status !== "pending"; });
  }

  // refundPolicy says what the manager may set when approving a cancellation: before any station
  // started the whole amount goes back; afterwards anything from nothing to the whole amount.
  function refundPolicy(order) {
    if (workStarted(order)) return { fixed: false, min: 0, max: order.total, suggested: 0 };
    return { fixed: true, min: order.total, max: order.total, suggested: order.total };
  }

  // summary counts orders and money for the manager's tiles.
  function summary(orders) {
    var s = { orders: orders.length, completed: 0, cancelled: 0, active: 0, takings: 0, refunded: 0 };
    orders.forEach(function (o) {
      if (o.status === "completed") s.completed++;
      else if (o.status === "cancelled") s.cancelled++;
      else s.active++;
      if (o.payment) s.takings += o.payment.amount;
      s.refunded += o.refunded || 0;
    });
    s.net = s.takings - s.refunded;
    return s;
  }

  // ageText says how long ago something happened.
  function ageText(iso, now) {
    var secs = Math.max(0, Math.round(((now || Date.now()) - Date.parse(iso)) / 1000));
    if (secs < 60) return T("just now");
    var mins = Math.floor(secs / 60);
    if (mins < 60) return T("{n} min", { n: mins });
    var h = Math.floor(mins / 60);
    return T("{h} h {m} min", { h: h, m: mins % 60 });
  }

  // agoText is ageText as a time in the past: "5 min ago", or "just now".
  function agoText(iso, now) {
    var secs = Math.max(0, Math.round(((now || Date.now()) - Date.parse(iso)) / 1000));
    return secs < 60 ? T("just now") : T("{age} ago", { age: ageText(iso, now) });
  }

  // statusLabel says in words where an order is.
  function statusLabel(s) {
    switch (s) {
      case "placed": return T("Waiting for payment");
      case "paid": return T("Paid, waiting for the kitchen");
      case "preparing": return T("Being made");
      case "ready": return T("Ready");
      case "completed": return T("Handed over");
      case "cancelled": return T("Cancelled");
    }
    return s;
  }

  // cashChange is the change due, or null when the amount handed over does not cover the total.
  function cashChange(total, tendered) {
    return tendered >= total ? tendered - total : null;
  }

  // quickAmounts are the notes a cashier most likely received for a total: the exact amount and
  // the next round amounts above it.
  function quickAmounts(total) {
    var out = [total];
    [10000, 20000, 50000, 100000, 200000, 500000].forEach(function (step) {
      var up = Math.ceil(total / step) * step;
      if (up > total && out.indexOf(up) < 0 && out.length < 5) out.push(up);
    });
    return out;
  }

  // newCommandID makes an ID for a command. It must be 8 to 64 letters, digits, dots, dashes or
  // underscores. rand returns n random bytes (the browser's crypto, or a stand-in in a test).
  function newCommandID(prefix, rand) {
    var bytes = rand(8), hex = "";
    for (var i = 0; i < bytes.length; i++) hex += ("0" + (bytes[i] & 255).toString(16)).slice(-2);
    return String(prefix).replace(/[^A-Za-z0-9._-]/g, "").slice(0, 24) + "-" + hex;
  }

  // parseStream splits the text of a server-sent stream into events. It returns the complete
  // events and the text of an event that has not ended yet, to be put in front of the next chunk.
  function parseStream(buffer) {
    var events = [];
    var parts = buffer.replace(/\r\n/g, "\n").split("\n\n");
    var rest = parts.pop();
    parts.forEach(function (block) {
      var ev = { event: "message", data: "", id: "" }, any = false;
      block.split("\n").forEach(function (line) {
        if (!line || line[0] === ":") return;
        var i = line.indexOf(":");
        var field = i < 0 ? line : line.slice(0, i);
        var value = i < 0 ? "" : line.slice(i + 1).replace(/^ /, "");
        if (field === "event") { ev.event = value; any = true; }
        else if (field === "data") { ev.data += (ev.data ? "\n" : "") + value; any = true; }
        else if (field === "id") { ev.id = value; any = true; }
      });
      if (any) events.push(ev);
    });
    return { events: events, rest: rest };
  }

  // readySince is when an order became ready: the time its last station finished, or when it was
  // placed if the order does not say.
  function readySince(o) {
    var best = "";
    (o.tickets || []).forEach(function (t) { if (t.ready_at && (!best || Date.parse(t.ready_at) > Date.parse(best))) best = t.ready_at; });
    return best || o.placed_at;
  }

  // screensFor lists the screens a person may use, from the permissions that /api/me reports.
  function screensFor(perms) {
    var can = function (p) { return !!(perms && perms[p]); };
    var out = [];
    if (can("order.place")) out.push("order");
    if (can("order.view_own") && !can("order.view_all")) out.push("myorders");
    if (can("order.record_payment")) out.push("till");
    if (can("shift.open") || can("shift.close") || can("shift.review")) out.push("shift");
    if (can("order.hand_over")) out.push("runner");
    if (can("ticket.start") || can("ticket.finish")) out.push("kitchen");
    if (can("order.approve_cancel") || can("order.issue_refund")) out.push("manager");
    if (can("stock.view")) out.push("stock");
    if (can("menu.manage")) out.push("menu");
    if (can("promo.manage")) out.push("promotions");
    if (can("report.view")) out.push("reports");
    if (can("user.manage")) out.push("users");
    return out;
  }

  // ---- accounts -------------------------------------------------------------------------

  // userState says in one word what is the matter with an account, if anything: the most serious
  // thing first. now is a time in milliseconds, for a lock that has run out.
  function userState(u, now) {
    if (u.disabled) return { key: "disabled", label: T("disabled") };
    if (u.locked_until && Date.parse(u.locked_until) > (now === undefined ? Date.now() : now)) return { key: "locked", label: T("locked") };
    if (u.must_change_password) return { key: "change", label: T("must change password") };
    return { key: "active", label: T("active") };
  }

  // passwordProblem checks a new password before it is sent, to say at once what the service
  // would say. The service decides; this only saves a round trip. It returns "" when nothing is
  // wrong that can be seen here.
  function passwordProblem(pw, again, loginName) {
    if (Array.from(pw).length < 10) return T("Use at least 10 characters.");
    if (new TextEncoder().encode(pw).length > 128) return T("Use at most 128 bytes.");
    if (loginName && loginName.length >= 3 && pw.toLowerCase().indexOf(loginName.toLowerCase()) >= 0) return T("The password cannot contain your login name.");
    if (new Set(Array.from(pw)).size < 5) return T("Use at least 5 different characters.");
    if (again !== undefined && pw !== again) return T("The two passwords differ.");
    return "";
  }

  // One sentence for each kind of line of the trail. actor is who did it (empty when nobody was
  // signed in) and subject is the account it was done to.
  var EVENTS = {
    "login.success": function (e) { return T("{subject} signed in", e); },
    "login.failure": function (e) { return T("Wrong password for {subject}", e); },
    "login.locked": function (e) { return T("{subject} was locked after wrong passwords", e); },
    "login.blocked": function (e) { return T("Sign-in to {subject} refused: the account is locked", e); },
    "login.disabled": function (e) { return T("Sign-in to {subject} refused: the account is disabled", e); },
    "login.unknown": function (e) {
      var tried = (e.detail && e.detail.login) || e.subject;
      return tried ? T("Sign-in with a login name that does not exist: {name}", { name: tried }) : T("Sign-in with a login name that does not exist");
    },
    "logout": function (e) { return T("{subject} signed out", e); },
    "password.change": function (e) { return T("{subject} changed their password", e); },
    "password.wrong_current": function (e) { return T("{subject} gave a wrong current password", e); },
    "user.create": function (e) { return T("{actor} created {subject}", e); },
    "user.update": function (e) { return T("{actor} changed {subject}", e); },
    "user.disable": function (e) { return T("{actor} disabled {subject}", e); },
    "user.enable": function (e) { return T("{actor} enabled {subject}", e); },
    "user.unlock": function (e) { return T("{actor} unlocked {subject}", e); },
    "user.reset_password": function (e) { return T("{actor} reset the password of {subject}", e); },
    "user.set_password": function (e) { return T("The password of {subject} was set at the command line", e); },
    "session.revoke": function (e) { return T("{actor} ended a session of {subject}", e); },
    "session.revoke_all": function (e) { return T("{actor} ended all sessions of {subject}", e); },
  };

  // describeEvent turns a line of the trail into a sentence. An action that is not known is shown
  // as it is: a new kind of line must not disappear from the screen.
  function describeEvent(e) {
    var f = EVENTS[e.action];
    return f ? f(e) : [e.actor, e.action, e.subject].filter(Boolean).join(" ");
  }

  // shortAgent makes a browser's long user-agent into "Chrome on Windows". It is a convenience
  // for reading a list of sessions, not a way to identify anything.
  function shortAgent(ua) {
    if (!ua) return T("unknown browser");
    var browser = /Edg\//.test(ua) ? "Edge" : /OPR\/|Opera/.test(ua) ? "Opera" : /Firefox\//.test(ua) ? "Firefox" :
      /Chrome\//.test(ua) ? "Chrome" : /Safari\//.test(ua) ? "Safari" : /curl\//.test(ua) ? "curl" : T("browser");
    var os = /Windows/.test(ua) ? "Windows" : /Android/.test(ua) ? "Android" : /iPhone|iPad/.test(ua) ? "iOS" :
      /Mac OS X/.test(ua) ? "macOS" : /Linux/.test(ua) ? "Linux" : "";
    return os ? T("{browser} on {os}", { browser: browser, os: os }) : browser;
  }

  return {
    formatMoney: formatMoney, groupProblem: groupProblem, itemProblems: itemProblems, unitPrice: unitPrice,
    optionNames: optionNames, toggleOption: toggleOption, cartEstimate: cartEstimate, cartBody: cartBody,
    describeError: describeError, groupForTill: groupForTill, stationWork: stationWork, stationsOf: stationsOf,
    cancelQueue: cancelQueue, refundPolicy: refundPolicy, workStarted: workStarted, summary: summary,
    ageText: ageText, agoText: agoText, statusLabel: statusLabel, cashChange: cashChange, quickAmounts: quickAmounts,
    newCommandID: newCommandID, parseStream: parseStream, screensFor: screensFor, readySince: readySince, ACTIVE: ACTIVE,
    userState: userState, passwordProblem: passwordProblem, describeEvent: describeEvent, shortAgent: shortAgent,
  };
});
