"use strict";
// Run with: node --test internal/orderservice/web/logic.test.js
const test = require("node:test");
const assert = require("node:assert/strict");
const L = require("./logic.js");

// The menu below is invented for these tests.
const coffee = {
  id: "coffee", name: "Coffee", category: "drinks", station: "drinks", price: 29000,
  groups: [
    { id: "size", name: "Size", min: 1, max: 1, options: [{ id: "m", name: "Medium", delta: 0 }, { id: "l", name: "Large", delta: 8000 }] },
    { id: "extra", name: "Extras", min: 0, max: 2, options: [{ id: "shot", name: "Extra shot", delta: 5000 }, { id: "pearl", name: "Pearls", delta: 7000 }, { id: "jelly", name: "Jelly", delta: 6000 }] },
  ],
};
const bun = { id: "bun", name: "Bun", category: "food", station: "hot", price: 25000, groups: [] };
const menu = {
  items: [coffee, bun],
  combos: [{ id: "morning", name: "Morning", price: 45000, slots: [{ name: "Drink", items: ["coffee"] }, { name: "Food", items: ["bun"] }] }],
};

test("money is written like the service writes it", () => {
  assert.equal(L.formatMoney(0), "0đ");
  assert.equal(L.formatMoney(999), "999đ");
  assert.equal(L.formatMoney(1000), "1.000đ");
  assert.equal(L.formatMoney(89100), "89.100đ");
  assert.equal(L.formatMoney(1234567), "1.234.567đ");
  assert.equal(L.formatMoney(-9900), "-9.900đ");
});

test("a group that needs one choice says so until one is made", () => {
  const size = coffee.groups[0];
  assert.equal(L.groupProblem(size, {}), "Choose one: Size");
  assert.equal(L.groupProblem(size, { size: ["l"] }), "");
  assert.match(L.groupProblem(size, { size: ["m", "l"] }), /at most 1/);
  assert.deepEqual(L.itemProblems(coffee, {}), ["Choose one: Size"]);
  assert.deepEqual(L.itemProblems(bun, {}), []);
  assert.equal(L.groupProblem({ id: "g", name: "G", min: 2, max: 3, options: [] }, { g: ["a"] }), "Choose at least 2: G");
});

test("the price of an item adds the options that are picked", () => {
  assert.equal(L.unitPrice(coffee, { size: ["m"] }), 29000);
  assert.equal(L.unitPrice(coffee, { size: ["l"], extra: ["shot", "pearl"] }), 29000 + 8000 + 5000 + 7000);
  assert.equal(L.unitPrice(bun, {}), 25000);
  assert.deepEqual(L.optionNames(coffee, { size: ["l"], extra: ["pearl", "shot"] }), ["Large", "Extra shot", "Pearls"]);
});

test("clicking an option replaces it in a one-choice group, toggles it in a group of several, and never changes the choice it was given", () => {
  const size = coffee.groups[0], extra = coffee.groups[1];
  const start = { size: ["m"] };
  const a = L.toggleOption(start, size, "l");
  assert.deepEqual(a, { size: ["l"] });
  assert.deepEqual(start, { size: ["m"] });
  assert.deepEqual(L.toggleOption(a, size, "l"), { size: ["l"] }, "a required choice cannot be unpicked");
  const b = L.toggleOption(a, extra, "shot");
  const c = L.toggleOption(b, extra, "pearl");
  assert.deepEqual(c, { size: ["l"], extra: ["shot", "pearl"] });
  assert.deepEqual(L.toggleOption(c, extra, "jelly"), c, "a third extra is not added to a group of two");
  assert.deepEqual(L.toggleOption(c, extra, "shot"), { size: ["l"], extra: ["pearl"] });
  const optional = { id: "ice", name: "Ice", min: 0, max: 1, options: [{ id: "less", name: "Less", delta: 0 }] };
  assert.deepEqual(L.toggleOption({ ice: ["less"] }, optional, "less"), {}, "an optional choice can be unpicked");
});

test("the estimate of a cart counts items, options, combos and quantities", () => {
  const lines = [
    { item: "coffee", options: { size: ["l"] }, quantity: 2 },
    { item: "bun", options: {}, quantity: 1 },
    { combo: "morning", picks: [{ slot: 0, item: "coffee", options: { size: ["l"], extra: ["shot"] } }, { slot: 1, item: "bun", options: {} }], quantity: 1 },
  ];
  // 2 large coffees, a bun, and a combo at 45.000 plus the options of its coffee (8.000 + 5.000)
  assert.equal(L.cartEstimate(menu, lines), 2 * 37000 + 25000 + 45000 + 13000);
  assert.equal(L.cartEstimate(menu, []), 0);
});

test("the request that places an order carries only what is set", () => {
  const body = L.cartBody("cmd-1234abcd", [
    { item: "coffee", options: { size: ["l"] }, quantity: 2, note: "no straw" },
    { item: "bun", options: {}, quantity: 1 },
    { combo: "morning", picks: [{ slot: 0, item: "coffee", options: { size: ["m"] } }, { slot: 1, item: "bun", options: {} }], quantity: 1 },
  ], ["HELLO10"], "khach-1");
  assert.deepEqual(body, {
    command_id: "cmd-1234abcd",
    items: [
      { quantity: 2, note: "no straw", item: "coffee", options: { size: ["l"] } },
      { quantity: 1, item: "bun" },
      { quantity: 1, combo: "morning", picks: [{ slot: 0, item: "coffee", options: { size: ["m"] } }, { slot: 1, item: "bun" }] },
    ],
    codes: ["HELLO10"], customer: "khach-1",
  });
  assert.deepEqual(Object.keys(L.cartBody("cmd-1234abcd", [], [], "")), ["command_id", "items"]);
  assert.equal(L.cartBody("cmd-1234abcd", [], [], "", 0).points, undefined, "no points, nothing sent");
  assert.equal(L.cartBody("cmd-1234abcd", [], [], "", 12).points, 12);
});

test("an error is said in words, with what was short or which line was wrong", () => {
  assert.match(L.describeError(409, { code: "out_of_stock", error: "x", shortages: [{ ingredient: "bread", need: 2, have: 0 }] }), /bread \(needs 2, has 0\)/);
  assert.equal(L.describeError(422, { code: "cart", error: "x", lines: [{ line: 2, error: "menu: sold out: Tea" }] }), "Line 2: sold out: Tea");
  assert.match(L.describeError(503, { code: "unavailable", error: "x" }), /could not reach/);
  assert.equal(L.describeError(403, { code: "forbidden", error: "order: this person may not do that" }), "order: this person may not do that");
  assert.match(L.describeError(500, null), /500/);
});

const t0 = "2026-11-20T10:00:00+07:00";
function order(number, status, extra) {
  return Object.assign({ id: "ord-" + number, number: number, status: status, total: 50000, placed_at: t0, lines: [], tickets: [] }, extra || {});
}

test("the till has three columns, each in the order of the numbers", () => {
  const cols = L.groupForTill([order(5, "ready"), order(3, "placed"), order(2, "preparing"), order(1, "paid"), order(4, "placed"), order(6, "completed"), order(7, "cancelled")]);
  assert.deepEqual(cols.toPay.map((o) => o.number), [3, 4]);
  assert.deepEqual(cols.inKitchen.map((o) => o.number), [1, 2]);
  assert.deepEqual(cols.ready.map((o) => o.number), [5]);
});

test("a station sees its own pieces and notes, waiting or being made, and the oldest first", () => {
  const lines = [
    { name: "Coffee", quantity: 2, note: "no ice", pieces: [{ station: "drinks", name: "Coffee (Large)" }] },
    { name: "Bun", quantity: 1, note: "", pieces: [{ station: "hot", name: "Bun" }] },
    { name: "Tea", quantity: 1, note: "", pieces: [{ station: "drinks", name: "Tea" }] },
  ];
  const tk = (station, status, extra) => Object.assign({ station: station, status: status }, extra || {});
  const orders = [
    order(1, "paid", { lines: lines, tickets: [tk("drinks", "pending"), tk("hot", "pending")], payment: { at: "2026-11-20T10:05:00+07:00" } }),
    order(2, "preparing", { lines: lines, tickets: [tk("drinks", "started", { started_at: "2026-11-20T10:02:00+07:00" }), tk("hot", "ready")], payment: { at: "2026-11-20T10:01:00+07:00" } }),
    order(3, "paid", { lines: lines, tickets: [tk("drinks", "pending"), tk("hot", "pending")], payment: { at: "2026-11-20T10:03:00+07:00" }, cancel_request: { by: "x", reason: "y", at: t0 } }),
    order(4, "ready", { lines: lines, tickets: [tk("drinks", "ready"), tk("hot", "ready")] }),
    order(5, "placed", { lines: lines, tickets: [tk("drinks", "pending")] }),
  ];
  const drinks = L.stationWork(orders, "drinks");
  assert.deepEqual(drinks.waiting.map((w) => w.order.number), [3, 1], "oldest payment first");
  assert.deepEqual(drinks.cooking.map((w) => w.order.number), [2]);
  assert.deepEqual(drinks.waiting[1].pieces, [{ name: "Coffee (Large)", quantity: 2 }, { name: "Tea", quantity: 1 }]);
  assert.deepEqual(drinks.waiting[1].notes, ["no ice"]);
  assert.equal(drinks.waiting[0].cancelling, true);
  assert.equal(drinks.waiting[1].cancelling, false);
  const hot = L.stationWork(orders, "hot");
  assert.deepEqual(hot.waiting.map((w) => w.order.number), [3, 1]);
  assert.deepEqual(hot.cooking, []);
  assert.deepEqual(L.stationWork(orders, "cold"), { waiting: [], cooking: [] });
  assert.deepEqual(L.stationsOf(orders), ["drinks", "hot"]);
});

test("the manager's queue is the cancellation requests, oldest first, and the refund follows the rule", () => {
  const a = order(1, "paid", { cancel_request: { by: "a", reason: "r", at: "2026-11-20T10:10:00+07:00" }, tickets: [{ station: "hot", status: "pending" }] });
  const b = order(2, "preparing", { cancel_request: { by: "b", reason: "r", at: "2026-11-20T10:05:00+07:00" }, tickets: [{ station: "hot", status: "started" }] });
  assert.deepEqual(L.cancelQueue([a, order(3, "paid"), b]).map((o) => o.number), [2, 1]);
  assert.deepEqual(L.refundPolicy(a), { fixed: true, min: 50000, max: 50000, suggested: 50000 });
  assert.deepEqual(L.refundPolicy(b), { fixed: false, min: 0, max: 50000, suggested: 0 });
});

test("the summary counts orders and money", () => {
  const s = L.summary([
    order(1, "completed", { payment: { amount: 50000 } }),
    order(2, "cancelled", { payment: { amount: 50000 }, refunded: 50000 }),
    order(3, "completed", { payment: { amount: 50000 }, refunded: 10000 }),
    order(4, "placed"),
  ]);
  assert.deepEqual(s, { orders: 4, completed: 2, cancelled: 1, active: 1, takings: 150000, refunded: 60000, net: 90000 });
});

test("ages, statuses and cash", () => {
  const now = Date.parse("2026-11-20T10:30:00+07:00");
  assert.equal(L.ageText("2026-11-20T10:29:40+07:00", now), "just now");
  assert.equal(L.ageText("2026-11-20T10:25:00+07:00", now), "5 min");
  assert.equal(L.ageText("2026-11-20T08:50:00+07:00", now), "1 h 40 min");
  assert.equal(L.ageText("2026-11-20T11:00:00+07:00", now), "just now", "a time in the future is not negative");
  assert.equal(L.statusLabel("preparing"), "Being made");
  assert.equal(L.statusLabel("odd"), "odd");
  assert.equal(L.cashChange(89100, 100000), 10900);
  assert.equal(L.cashChange(89100, 89100), 0);
  assert.equal(L.cashChange(89100, 80000), null);
  assert.deepEqual(L.quickAmounts(89100), [89100, 90000, 100000, 200000, 500000]);
  assert.deepEqual(L.quickAmounts(100000), [100000, 200000, 500000]);
});

test("a command ID is safe to send and different every time", () => {
  let n = 0;
  const rand = (len) => Array.from({ length: len }, () => (n++ * 37) & 255);
  const a = L.newCommandID("pay ord/1", rand), b = L.newCommandID("pay ord/1", rand);
  assert.notEqual(a, b);
  for (const id of [a, b, L.newCommandID("x".repeat(100), rand)]) {
    assert.match(id, /^[A-Za-z0-9._-]{8,64}$/);
  }
});

test("the stream is cut into events, comments are ignored, and an unfinished event waits", () => {
  const text = "retry: 2000\n\nid: 13\nevent: order\ndata: {\"order\":\"ord-1\"}\n\n: keep-alive\n\nid: 14\nevent: stock\ndata: {\"offset\":14}\n\nid: 15\nevent: ord";
  const r = L.parseStream(text);
  assert.deepEqual(r.events, [
    { event: "order", data: '{"order":"ord-1"}', id: "13" },
    { event: "stock", data: '{"offset":14}', id: "14" },
  ]);
  assert.equal(r.rest, "id: 15\nevent: ord");
  const again = L.parseStream(r.rest + "er\ndata: {}\n\n");
  assert.deepEqual(again.events, [{ event: "order", data: "{}", id: "15" }]);
  assert.equal(again.rest, "");
  assert.deepEqual(L.parseStream("event: reset\r\ndata: {}\r\n\r\n").events, [{ event: "reset", data: "{}", id: "" }]);
});

test("each role gets the screens its permissions allow", () => {
  assert.deepEqual(L.screensFor({ "order.place": true, "order.view_own": true }), ["order", "myorders", "points"], "a customer has their orders and their points");
  assert.deepEqual(L.screensFor({ "order.place": true, "order.view_all": true, "order.record_payment": true, "order.hand_over": true }), ["order", "till", "runner"], "a cashier who also hands over has both screens");
  assert.deepEqual(L.screensFor({ "ticket.start": true, "ticket.finish": true, "order.view_all": true, "stock.view": true }), ["kitchen", "stock"]);
  assert.deepEqual(L.screensFor({ "order.hand_over": true, "order.view_all": true }), ["runner"], "a runner has a screen of their own, not the till");
  assert.deepEqual(L.screensFor({ "order.record_payment": true, "order.view_all": true }), ["till"]);
  assert.deepEqual(L.screensFor({ "order.approve_cancel": true, "stock.view": true }), ["manager", "stock"]);
  assert.deepEqual(L.screensFor({ "points.adjust": true, "order.view_all": true }), ["pointsdesk"], "a manager has the desk of points, and a customer has the points screen");
  assert.deepEqual(L.screensFor({ "user.manage": true, "audit.view": true }), ["users"], "an administrator has no kitchen screen, only the accounts");
  assert.deepEqual(L.screensFor(null), []);
});

test("an account says what is the matter with it, the most serious first", () => {
  const now = Date.parse("2026-11-20T10:00:00Z");
  assert.equal(L.userState({}, now).key, "active");
  assert.equal(L.userState({ must_change_password: true }, now).key, "change");
  assert.equal(L.userState({ must_change_password: true, locked_until: "2026-11-20T10:10:00Z" }, now).key, "locked");
  assert.equal(L.userState({ locked_until: "2026-11-20T09:59:00Z" }, now).key, "active", "a lock that has run out is not shown");
  assert.equal(L.userState({ disabled: true, locked_until: "2026-11-20T10:10:00Z", must_change_password: true }, now).key, "disabled");
});

test("a new password is checked here the way the service will check it", () => {
  assert.match(L.passwordProblem("short", "short", "thu-ngan"), /at least 10/);
  assert.match(L.passwordProblem("tám chữ", "tám chữ", "x"), /at least 10/, "characters are counted, not bytes");
  assert.equal(L.passwordProblem("tám chữ Việt đẹp", "tám chữ Việt đẹp", "thu-ngan"), "");
  assert.match(L.passwordProblem("my-Thu-Ngan-pass", "my-Thu-Ngan-pass", "thu-ngan"), /login name/);
  assert.match(L.passwordProblem("abababababab", "abababababab", "x"), /different characters/);
  assert.match(L.passwordProblem("a long enough one", "another long one", "x"), /differ/);
  assert.match(L.passwordProblem("é".repeat(70), "é".repeat(70), "x"), /128 bytes/);
  assert.equal(L.passwordProblem("a long enough one", undefined, "x"), "", "no confirmation to compare");
});

test("every line of the trail is a sentence, and an unknown one is still shown", () => {
  const line = (action, actor, subject, detail) => L.describeEvent({ action, actor, subject, detail });
  assert.equal(line("login.success", "thu-ngan", "thu-ngan"), "thu-ngan signed in");
  assert.equal(line("login.failure", "", "thu-ngan"), "Wrong password for thu-ngan");
  assert.equal(line("user.disable", "admin-1", "thu-ngan"), "admin-1 disabled thu-ngan");
  assert.equal(line("login.unknown", "", "", { login: "nobody" }), "Sign-in with a login name that does not exist: nobody");
  assert.equal(line("login.unknown", "", "", {}), "Sign-in with a login name that does not exist");
  assert.equal(line("user.set_password", "command-line", "admin-1"), "The password of admin-1 was set at the command line");
  assert.equal(line("shift.open", "cash-1", "till-2"), "cash-1 shift.open till-2");
});

test("a browser is named in a few words", () => {
  assert.equal(L.shortAgent("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"), "Chrome on Windows");
  assert.equal(L.shortAgent("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/124.0 Safari/537.36 Edg/124.0"), "Edge on Windows");
  assert.equal(L.shortAgent("Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605 Version/17.0 Mobile Safari/604"), "Safari on iOS");
  assert.equal(L.shortAgent("curl/8.4.0"), "curl");
  assert.equal(L.shortAgent(""), "unknown browser");
});

test("an order is ready from the moment its last station finished", () => {
  const o = { placed_at: "2026-11-20T10:00:00Z", tickets: [{ ready_at: "2026-11-20T10:05:00Z" }, { ready_at: "2026-11-20T10:09:00Z" }, {}] };
  assert.equal(L.readySince(o), "2026-11-20T10:09:00Z");
  assert.equal(L.readySince({ placed_at: "2026-11-20T10:00:00Z", tickets: [] }), "2026-11-20T10:00:00Z", "with no time of its own it is counted from when it was placed");
  assert.equal(L.readySince({ placed_at: "2026-11-20T10:00:00Z" }), "2026-11-20T10:00:00Z");
});

test("what the service says about points and signing up is written in a sentence", () => {
  assert.equal(L.describeError(409, { error: "x", code: "points", max: 7 }), "This order can use at most 7 points.");
  assert.equal(L.describeError(409, { error: "x", code: "points" }), "This order can use at most 0 points.", "no number means none");
  assert.equal(L.describeError(409, { error: "x", code: "points_short", max: 12 }), "Only 12 points can be taken off this customer.");
  assert.match(L.describeError(409, { error: "x", code: "no_points" }), /cannot be used/);
  assert.match(L.describeError(400, { error: "x", code: "no_customer" }), /no customer/);
  assert.match(L.describeError(403, { error: "x", code: "registration_closed" }), /Registration is closed/);
});

test("a login name is checked as the accounts check it", () => {
  assert.equal(L.loginProblem("linh.tran"), "");
  assert.equal(L.loginProblem("a1_"), "");
  for (const bad of ["", "ab", "two words", "-start", "UPPER", "x".repeat(33), "dấu"]) assert.notEqual(L.loginProblem(bad), "", bad);
});
