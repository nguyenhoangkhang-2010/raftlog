"use strict";
// Tests of what the shift screen works out before it sends or shows anything: a count as typed, what
// a drawer came out as, and what a shift was made of. Run with: node --test internal/orderservice/web/
const test = require("node:test");
const assert = require("node:assert/strict");
const I18N = require("./i18n.js");
const vi = require("./vi.js");
I18N.register("vi", vi);
const Shifts = require("./shifts.js");
const P = Shifts.pure(I18N.t);

test.afterEach(() => I18N.setLang("en"));

test("a count as typed becomes the counts the service takes and their total, with empty boxes left out", () => {
  const r = P.countsOf({ 500000: " 2 ", 100000: "3", 50000: "", 20000: "0", 1000: "7" });
  assert.equal(r.err, undefined);
  assert.deepEqual(r.counts, { 500000: 2, 100000: 3, 1000: 7 });
  assert.equal(r.total, 1_000_000 + 300_000 + 7_000);
  assert.deepEqual(P.countsOf({}), { counts: {}, total: 0 }, "an empty drawer is a count");
  assert.deepEqual(P.countsOf(null), { counts: {}, total: 0 });
});

test("a count that is not a number of pieces is refused, in the language", () => {
  for (const bad of ["1.5", "-1", "abc", "1e3", "100001", "2 000"]) {
    assert.match(P.countsOf({ 10000: bad }).err, /whole number of notes or coins/, JSON.stringify(bad));
  }
  I18N.setLang("vi");
  assert.match(P.countsOf({ 10000: "x" }).err, /số nguyên/);
});

test("a count that came from the service adds up", () => {
  assert.equal(P.totalOf({ 100000: 5, 50000: 4 }), 700_000);
  assert.equal(P.totalOf({}), 0);
  assert.equal(P.totalOf(undefined), 0);
});

test("a drawer is exact, short or over by the sign of its variance", () => {
  assert.equal(P.verdict(0).key, "exact");
  assert.equal(P.verdict(-20000).key, "short");
  assert.equal(P.verdict(5000).key, "over");
  assert.equal(P.verdict(-1).label, "The drawer is short.");
  I18N.setLang("vi");
  assert.equal(P.verdict(-1).label, "Két bị thiếu tiền.");
});

test("what a closed shift was made of adds up to what the drawer should have held", () => {
  const sh = { opening_total: 700_000, sales: 1_000_000, refunds: 120_000, pay_in: 200_000, pay_out: 50_000, expected: 1_730_000 };
  const rows = P.breakdown(sh);
  assert.deepEqual(rows.map((r) => r[0]), ["Opening count", "Cash sales", "Cash refunds", "Money put in", "Money taken out", "Should hold"]);
  const sum = rows.filter((r) => r[2] !== 0).reduce((s, r) => s + r[2] * r[1], 0);
  assert.equal(sum, sh.expected);
  assert.equal(rows[rows.length - 1][1], sh.expected);
  assert.deepEqual(P.breakdown({ opening_total: 5 }), [], "an open shift has no figures to show");
});

test("a shift needs a look when it is closed, off, and nobody has looked", () => {
  assert.equal(P.needsLook({ status: "closed", variance: -5000 }), true);
  assert.equal(P.needsLook({ status: "closed", variance: 100 }), true);
  assert.equal(P.needsLook({ status: "closed", variance: 0 }), false, "an exact drawer needs none");
  assert.equal(P.needsLook({ status: "closed", variance: -5000, reviewed_by: "mgr-1" }), false);
  assert.equal(P.needsLook({ status: "open" }), false);
});

test("whole numbers are read strictly", () => {
  assert.equal(P.intOf(" 12 "), 12);
  for (const s of ["", "1.5", "0x10", null, undefined]) assert.equal(P.intOf(s), null, String(s));
});
