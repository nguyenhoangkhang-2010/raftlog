"use strict";
// Tests of what the warehouse screen works out before it sends or shows anything: a delivery as
// typed, dates, prices in thousandths of a đồng, and what needs attention. Run with:
// node --test internal/orderservice/web/
const test = require("node:test");
const assert = require("node:assert/strict");
const I18N = require("./i18n.js");
const vi = require("./vi.js");
I18N.register("vi", vi);
const Warehouse = require("./warehouse.js");
const P = Warehouse.pure(I18N.t);

test.afterEach(() => I18N.setLang("en"));

const line = (ingredient, qty, cost, expires) => ({ ingredient, qty, cost, expires });

test("a delivery as typed becomes what the service takes, and empty lines are left out", () => {
  const r = P.receiptBody({
    supplier: "milk-co", document: "  INV-7 ",
    lines: [line(" milk ", " 500 ", "100000", "2026-12-31"), line("", "", "", ""), line("bread", "40", "", "")],
  });
  assert.equal(r.err, undefined);
  assert.deepEqual(r.body, {
    supplier: "milk-co", document: "INV-7",
    lines: [{ ingredient: "milk", qty: 500, total_cost: 100000, expires: "2026-12-31" }, { ingredient: "bread", qty: 40, total_cost: 0, expires: "" }],
  });
});

test("a delivery that makes no sense is refused with the number of the line, in the language", () => {
  const bad = (l) => P.receiptBody({ supplier: "", document: "", lines: [line("ok", "1", "", ""), l] }).err;
  assert.equal(bad(line("", "5", "", "")), "Line 2: choose an ingredient.");
  for (const q of ["", "0", "-3", "1.5", "abc"]) assert.match(bad(line("milk", q, "", "")), /^Line 2: the quantity/, "qty " + JSON.stringify(q));
  for (const c of ["-1", "1.5", "x", "1e3"]) assert.match(bad(line("milk", "5", c, "")), /^Line 2: the price/, "cost " + JSON.stringify(c));
  for (const d of ["31/12/2026", "2026-13-01", "2026-02-30", "tomorrow", "2026-1-5"]) assert.match(bad(line("milk", "5", "", d)), /^Line 2: the date/, "date " + d);
  assert.equal(P.receiptBody({ supplier: "", document: "", lines: [line("", "", "", "")] }).err, "Add at least one line to the delivery.");
  I18N.setLang("vi");
  assert.equal(bad(line("", "5", "", "")), "Dòng 2: hãy chọn nguyên liệu.");
});

test("a date is a real day of the calendar", () => {
  assert.ok(P.validDate("2026-12-31"));
  assert.ok(P.validDate("2028-02-29"), "a leap year");
  assert.ok(!P.validDate("2026-02-29"));
  assert.ok(!P.validDate("2026-04-31"));
  assert.ok(!P.validDate("2026-12-31 "));
  assert.ok(!P.validDate(""));
});

test("adding days to a date does not depend on the time zone and carries over months and years", () => {
  assert.equal(P.addDays("2026-11-14", 3), "2026-11-17");
  assert.equal(P.addDays("2026-11-30", 1), "2026-12-01");
  assert.equal(P.addDays("2026-12-31", 1), "2027-01-01");
  assert.equal(P.addDays("2028-02-28", 1), "2028-02-29");
  assert.equal(P.addDays("2026-03-01", -1), "2026-02-28");
  assert.equal(P.addDays("2026-11-14", 180), "2027-05-13");
});

test("prices are kept in thousandths of a dong and shown in dong", () => {
  assert.equal(P.milliToDong(80_000_000), 80000);
  assert.equal(P.milliToDong(500), 1, "half a dong rounds up");
  assert.equal(P.milliToDong(499), 0);
  assert.equal(P.unitCost(0), "");
  assert.equal(P.unitCost(2_000_000), "2000");
  assert.equal(P.unitCost(500), "0.5");
  assert.equal(P.unitCost(3333), "3.33");
  assert.equal(P.unitCost(1250), "1.25");
});

test("the first date at which a lot of an ingredient expires, and what needs attention", () => {
  const row = { lots: [{ expires_at: "2026-12-05T00:00:00Z" }, { expires_at: "2026-11-25T00:00:00Z" }, {}] };
  assert.equal(P.soonest(row), "2026-11-25T00:00:00Z");
  assert.equal(P.soonest({ lots: [{}] }), "");
  assert.equal(P.soonest({}), "");
  assert.deepEqual(P.alerts([{ low: true }, { soon: 5 }, { expired: 2, low: true }, {}]), { low: 2, soon: 1, expired: 1 });
});

test("what a count found is against what the stock said, and the last day of a lot is the day before it stops", () => {
  assert.equal(P.variance({ counted: 93, expected: 100 }), -7);
  assert.equal(P.variance({ counted: 10, expected: 8 }), 2);
  assert.equal(P.variance({ counted: 5, expected: 5 }), 0);
  const stops = new Date(2026, 10, 26, 0, 0, 0); // the start of the 26th in the browser's own zone
  assert.equal(P.lastDay(stops.toISOString()), "2026-11-25");
});

test("whole numbers are read strictly", () => {
  assert.equal(P.intOf(" 12 "), 12);
  assert.equal(P.intOf("-4"), -4);
  for (const s of ["", "1.5", "1e3", "0x10", "twelve", null, undefined]) assert.equal(P.intOf(s), null, String(s));
});
