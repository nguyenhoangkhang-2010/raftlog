"use strict";
// Tests of what the reports screen works out for itself: the days of a named period, the totals of the
// sales, the height of a bar, and how long is written. Run with: node --test internal/orderservice/web/
const test = require("node:test");
const assert = require("node:assert/strict");
const I18N = require("./i18n.js");
const vi = require("./vi.js");
I18N.register("vi", vi);
const Reports = require("./reports.js");
const P = Reports.pure(I18N.t);

test.afterEach(() => I18N.setLang("en"));

test("the days of a named period are counted back from today", () => {
  assert.deepEqual(P.preset("today", "2026-11-14"), { from: "2026-11-14", to: "2026-11-14" });
  assert.deepEqual(P.preset("yesterday", "2026-11-14"), { from: "2026-11-13", to: "2026-11-13" });
  assert.deepEqual(P.preset("week", "2026-11-14"), { from: "2026-11-08", to: "2026-11-14" }, "seven days with today");
  assert.deepEqual(P.preset("30", "2026-11-14"), { from: "2026-10-16", to: "2026-11-14" });
  assert.deepEqual(P.preset("month", "2026-11-14"), { from: "2026-11-01", to: "2026-11-14" });
  assert.deepEqual(P.preset("lastmonth", "2026-11-14"), { from: "2026-10-01", to: "2026-10-31" });
});

test("a period that crosses a month or a year, or a leap day, is counted right", () => {
  assert.deepEqual(P.preset("week", "2027-01-03"), { from: "2026-12-28", to: "2027-01-03" });
  assert.deepEqual(P.preset("lastmonth", "2027-01-10"), { from: "2026-12-01", to: "2026-12-31" });
  assert.deepEqual(P.preset("lastmonth", "2028-03-05"), { from: "2028-02-01", to: "2028-02-29" });
  assert.equal(P.addDays("2028-02-28", 1), "2028-02-29");
  assert.equal(P.addDays("2026-03-01", -1), "2026-02-28");
});

test("a date is written from the local day, with two digits", () => {
  assert.equal(P.iso(new Date(2026, 0, 5, 23, 59)), "2026-01-05");
  assert.equal(P.iso(new Date(2026, 10, 14, 0, 1)), "2026-11-14");
});

test("the totals of the days add up and the average is of the collected money per order", () => {
  const t = P.totals([
    { orders: 3, subtotal: 150000, discount: 15000, total: 135000, cash: 100000, card: 35000, transfer: 0, refunds: 10000, net: 125000 },
    { orders: 1, subtotal: 45000, discount: 0, total: 45000, cash: 0, card: 0, transfer: 45000, refunds: 0, net: 45000 },
  ]);
  assert.deepEqual(t, { orders: 4, subtotal: 195000, discount: 15000, total: 180000, cash: 100000, card: 35000, transfer: 45000, refunds: 10000, net: 170000, average: 45000 });
  assert.equal(P.totals([]).average, 0, "no orders, no average, and no division by zero");
  assert.equal(P.totals([{ orders: 0 }]).total, 0, "a day with fields missing counts as nothing");
});

test("a bar is as tall as its share of the tallest, and no value gives no bar", () => {
  assert.deepEqual(P.scale([0, 50, 100], 100), [0, 50, 100]);
  assert.deepEqual(P.scale([1, 1000], 100), [1, 100], "a small value still shows");
  assert.deepEqual(P.scale([0, 0], 100), [0, 0]);
  assert.deepEqual(P.scale([], 100), []);
  assert.deepEqual(P.scale([-5, 10], 10), [0, 10], "a negative day is not drawn downward");
});

test("a time to make an order is written in minutes and seconds", () => {
  assert.equal(P.duration(0), "–");
  assert.equal(P.duration(45), "45 s");
  assert.equal(P.duration(125), "2 min 5 s");
  I18N.setLang("vi");
  assert.equal(P.duration(125), "2 phút 5 giây");
});

test("a share is a whole percentage, and empty when there is no whole", () => {
  assert.equal(P.share(25, 100), "25%");
  assert.equal(P.share(1, 3), "33%");
  assert.equal(P.share(5, 0), "");
});
