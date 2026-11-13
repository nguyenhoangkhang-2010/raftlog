// The parts of the screens that are plain functions: money, prices, the choices of an item,
// the way orders are grouped for each screen, and the parsing of the stream of changes. They
// touch no page, so they run in the browser and, for the tests, in Node.
(function (root, factory) {
  if (typeof module === "object" && module.exports) module.exports = factory();
  else root.KitchenLogic = factory();
})(typeof self !== "undefined" ? self : this, function () {
  "use strict";

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
      return group.min === 1 && group.max === 1 ? "Choose one: " + group.name : "Choose at least " + group.min + ": " + group.name;
    }
    if (n > group.max) return "Choose at most " + group.max + ": " + group.name;
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

  // describeError turns the service's answer into a sentence for a person.
  function describeError(status, body) {
    body = body || {};
    var msg = body.error || "Something went wrong (" + status + ")";
    if (body.code === "out_of_stock" && body.shortages && body.shortages.length) {
      msg = "Not enough in stock: " + body.shortages.map(function (s) {
        return s.ingredient + " (needs " + s.need + ", has " + s.have + ")";
      }).join(", ");
    } else if (body.code === "cart" && body.lines && body.lines.length) {
      msg = body.lines.map(function (l) { return "Line " + l.line + ": " + l.error.replace(/^menu: /, ""); }).join("; ");
    } else if (body.code === "unavailable") {
      msg = "The kitchen system could not reach its log. Try again in a moment.";
    } else if (body.code === "unauthorized") {
      msg = "The token was not accepted.";
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
    if (secs < 60) return "just now";
    var mins = Math.floor(secs / 60);
    if (mins < 60) return mins + " min";
    var h = Math.floor(mins / 60);
    return h + " h " + (mins % 60) + " min";
  }

  var STATUS = {
    placed: "Waiting for payment", paid: "Paid, waiting for the kitchen", preparing: "Being made",
    ready: "Ready", completed: "Handed over", cancelled: "Cancelled",
  };
  function statusLabel(s) { return STATUS[s] || s; }

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

  // screensFor lists the screens a person may use, from the permissions that /api/me reports.
  function screensFor(perms) {
    var can = function (p) { return !!(perms && perms[p]); };
    var out = [];
    if (can("order.place")) out.push("order");
    if (can("order.view_own") && !can("order.view_all")) out.push("myorders");
    if (can("order.record_payment") || can("order.hand_over")) out.push("till");
    if (can("ticket.start") || can("ticket.finish")) out.push("kitchen");
    if (can("order.approve_cancel") || can("order.issue_refund")) out.push("manager");
    if (can("stock.view")) out.push("stock");
    return out;
  }

  return {
    formatMoney: formatMoney, groupProblem: groupProblem, itemProblems: itemProblems, unitPrice: unitPrice,
    optionNames: optionNames, toggleOption: toggleOption, cartEstimate: cartEstimate, cartBody: cartBody,
    describeError: describeError, groupForTill: groupForTill, stationWork: stationWork, stationsOf: stationsOf,
    cancelQueue: cancelQueue, refundPolicy: refundPolicy, workStarted: workStarted, summary: summary,
    ageText: ageText, statusLabel: statusLabel, cashChange: cashChange, quickAmounts: quickAmounts,
    newCommandID: newCommandID, parseStream: parseStream, screensFor: screensFor, ACTIVE: ACTIVE,
  };
});
