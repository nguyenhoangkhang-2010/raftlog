"use strict";
// Tests of what the points screen works out for itself: how many points an order may use, what is checked before
// an order is sent, and how an entry of the history is written. Run with: node --test internal/orderservice/web/
const test = require("node:test");
const assert = require("node:assert/strict");
const I18N = require("./i18n.js");
const vi = require("./vi.js");
I18N.register("vi", vi);
const Loyalty = require("./loyalty.js");
const P = Loyalty.pure(I18N.t);

test.afterEach(() => I18N.setLang("en"));

const pts = { enabled: true, earn_per: 10000, value: 100, max_share: 50, available: 40, balance: 45, held: 5 };

test("an order may use half of its total in points, no more than the customer has", () => {
  assert.equal(P.maxUsable(45000, pts), 40, "half of 45.000đ is 225 points; the customer has 40");
  assert.equal(P.maxUsable(2000, pts), 10, "half of 2.000đ at 100đ a point");
  assert.equal(P.maxUsable(45000, { ...pts, available: 1000 }), 225);
  assert.equal(P.maxUsable(45000, { ...pts, max_share: 100, available: 1000 }), 450);
  assert.equal(P.maxUsable(99, pts), 0, "an order too small for a point");
  assert.equal(P.maxUsable(0, pts), 0);
  assert.equal(P.maxUsable(45000, { ...pts, available: 0 }), 0);
  assert.equal(P.maxUsable(45000, { ...pts, enabled: false }), 0);
  assert.equal(P.maxUsable(45000, null), 0);
});

test("the points typed for an order are checked before it is sent", () => {
  assert.equal(P.pointsProblem("", pts, 45000), "", "an empty box is none");
  assert.equal(P.pointsProblem("0", pts, 45000), "");
  assert.equal(P.pointsProblem(" 12 ", pts, 45000), "");
  assert.equal(P.pointsProblem("40", pts, 45000), "");
  assert.match(P.pointsProblem("abc", pts, 45000), /whole number/);
  assert.match(P.pointsProblem("1.5", pts, 45000), /whole number/);
  assert.match(P.pointsProblem("-3", pts, 45000), /whole number/);
  assert.match(P.pointsProblem("41", pts, 45000), /40 points available/);
  assert.match(P.pointsProblem("11", pts, 2000), /at most 10/, "the order is too small for 11");
});

test("a whole number is read, and nothing else", () => {
  assert.equal(P.intOf("42"), 42);
  assert.equal(P.intOf(" 7 "), 7);
  assert.equal(P.intOf(""), null);
  assert.equal(P.intOf("4 2"), null);
  assert.equal(P.intOf("0x10"), null);
  assert.equal(P.intOf(undefined), null);
});

test("an entry of the history says what happened, and signs the points", () => {
  assert.equal(P.kindLabel("earned"), "Earned");
  assert.equal(P.kindLabel("redeemed"), "Used");
  assert.equal(P.kindLabel("returned"), "Given back");
  assert.equal(P.kindLabel("taken_back"), "Taken back");
  assert.equal(P.kindLabel("something new"), "something new", "a kind that is not known shows as it is");
  assert.equal(P.signed(8), "+8");
  assert.equal(P.signed(-5), "−5");
  assert.equal(P.signed(0), "0");
  I18N.setLang("vi");
  assert.equal(P.kindLabel("earned"), "Được cộng");
});

test("the rules of the program are written from what the service says", () => {
  const money = (n) => n + "đ";
  const [a, b] = P.rule(pts, money);
  assert.equal(a, "Every 10000đ you pay earns one point.");
  assert.equal(b, "One point takes 100đ off an order, and points may pay for up to 50% of it.");
});
