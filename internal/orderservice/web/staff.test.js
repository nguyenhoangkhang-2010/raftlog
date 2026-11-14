"use strict";
// Tests of what the schedule screen works out for itself: weeks and days, how long is written, what is
// checked before a shift, an entry or a rate is sent, which shift is next, and which rate is in force.
// Run with: node --test internal/orderservice/web/
const test = require("node:test");
const assert = require("node:assert/strict");
const I18N = require("./i18n.js");
const vi = require("./vi.js");
I18N.register("vi", vi);
const Staff = require("./staff.js");
const P = Staff.pure(I18N.t);

test.afterEach(() => I18N.setLang("en"));

test("a week starts on Monday, whichever day it is asked from", () => {
  assert.equal(P.weekStart("2026-11-16"), "2026-11-16", "a Monday is its own week's start");
  assert.equal(P.weekStart("2026-11-14"), "2026-11-09", "a Saturday");
  assert.equal(P.weekStart("2026-11-22"), "2026-11-16", "a Sunday belongs to the week before");
  assert.equal(P.weekStart("2026-01-01"), "2025-12-29", "across a year");
  assert.equal(P.weekStart("2028-03-01"), "2028-02-28", "across a leap day");
  assert.deepEqual(P.weekDays("2026-11-16"), ["2026-11-16", "2026-11-17", "2026-11-18", "2026-11-19", "2026-11-20", "2026-11-21", "2026-11-22"]);
  assert.equal(P.weekDays("2026-12-28")[6], "2027-01-03");
});

test("a day is written as its weekday and its day and month, in the language of the page", () => {
  assert.equal(P.dayLabel("2026-11-16"), "Mon 16/11");
  assert.equal(P.dayLabel("2026-11-22"), "Sun 22/11");
  assert.equal(P.shortDay("2026-03-05"), "05/03");
  I18N.setLang("vi");
  assert.equal(P.dayLabel("2026-11-16"), "T2 16/11");
  assert.equal(P.dayLabel("2026-11-22"), "CN 22/11");
});

test("a length of time is written in hours and minutes, and a difference carries its sign", () => {
  assert.equal(P.minutesText(0), "0 min");
  assert.equal(P.minutesText(45), "45 min");
  assert.equal(P.minutesText(60), "1 h");
  assert.equal(P.minutesText(215), "3 h 35 min");
  assert.equal(P.minutesText(-5), "0 min", "a negative length is not written");
  assert.equal(P.signedMinutes(0), "0");
  assert.equal(P.signedMinutes(20), "+20 min");
  assert.equal(P.signedMinutes(-65), "−1 h 5 min");
  I18N.setLang("vi");
  assert.equal(P.minutesText(215), "3 giờ 35 phút");
});

test("a shift is written by its times, and one that runs past midnight says so", () => {
  assert.equal(P.shiftText({ start: "08:00", end: "16:30" }), "08:00–16:30");
  assert.equal(P.shiftText({ start: "18:00", end: "02:00" }), "18:00–02:00 (next day)");
  I18N.setLang("vi");
  assert.equal(P.shiftText({ start: "18:00", end: "02:00" }), "18:00–02:00 (sang hôm sau)");
});

test("a shift is checked before it is sent, as the service would check it", () => {
  const ok = { user: "an", day: "2026-11-16", start: "08:00", end: "16:00", station: "line" };
  assert.equal(P.shiftProblem(ok), "");
  assert.equal(P.shiftProblem({ ...ok, start: "18:00", end: "02:00" }), "", "a night shift is fine");
  assert.match(P.shiftProblem({ ...ok, user: "" }), /who works/);
  assert.match(P.shiftProblem({ ...ok, day: "" }), /day/);
  assert.match(P.shiftProblem({ ...ok, start: "8:00" }), /start and the end/);
  assert.match(P.shiftProblem({ ...ok, end: "" }), /start and the end/);
  assert.match(P.shiftProblem({ ...ok, end: "08:20" }), /30 minutes to 16 hours/, "too short");
  assert.match(P.shiftProblem({ ...ok, start: "06:00", end: "23:00" }), /30 minutes to 16 hours/, "too long");
  assert.match(P.shiftProblem({ ...ok, end: "08:00" }), /30 minutes to 16 hours/, "the same time is a day, which is too long");
  assert.match(P.shiftProblem({ ...ok, station: "x".repeat(21) }), /20 characters/);
  assert.equal(P.shiftProblem({ ...ok, start: "08:00", end: "08:30" }), "", "thirty minutes is the shortest");
  assert.equal(P.shiftProblem({ ...ok, start: "06:00", end: "22:00" }), "", "sixteen hours is the longest");
});

test("an entry that a manager writes down needs a day, two times and a reason; an open one may lack the end", () => {
  const ok = { user: "an", day: "2026-11-16", in: "08:00", out: "12:00", reason: "forgot" };
  assert.equal(P.entryProblem(ok, false), "");
  assert.match(P.entryProblem({ ...ok, user: "" }, false), /who worked/);
  assert.match(P.entryProblem({ ...ok, day: "16/11" }, false), /day/);
  assert.match(P.entryProblem({ ...ok, out: "" }, false), /start and the end/);
  assert.equal(P.entryProblem({ ...ok, out: "" }, true), "", "a stretch that is still open has no end");
  assert.match(P.entryProblem({ ...ok, reason: "  " }, false), /why/);
  const fix = { day: "2026-11-16", in: "08:00", out: "12:00", reason: "door log" };
  assert.equal(P.entryProblem(fix, false), "", "a correction names no one");
});

test("a rate is a whole number of đồng, within the limit, from a day", () => {
  const ok = { user: "an", from: "2026-11-01", rate: "30000" };
  assert.equal(P.rateProblem(ok, 10000000), "");
  assert.equal(P.rateProblem({ ...ok, rate: "0" }, 10000000), "", "a rate of nothing is allowed");
  assert.match(P.rateProblem({ ...ok, user: "" }, 10000000), /who/);
  assert.match(P.rateProblem({ ...ok, from: "" }, 10000000), /day/);
  assert.match(P.rateProblem({ ...ok, rate: "-5" }, 10000000), /whole number/);
  assert.match(P.rateProblem({ ...ok, rate: "12.5" }, 10000000), /whole number/);
  assert.match(P.rateProblem({ ...ok, rate: "" }, 10000000), /whole number/);
  assert.match(P.rateProblem({ ...ok, rate: "10000001" }, 10000000), /10000000/);
});

test("the next shift is the first that has not ended", () => {
  const at = (s) => Date.parse(s);
  const shifts = [
    { id: 1, starts_at: "2026-11-16T08:00:00Z", ends_at: "2026-11-16T12:00:00Z" },
    { id: 3, starts_at: "2026-11-17T08:00:00Z", ends_at: "2026-11-17T12:00:00Z" },
    { id: 2, starts_at: "2026-11-16T14:00:00Z", ends_at: "2026-11-16T18:00:00Z" },
  ];
  assert.equal(P.nextShift(shifts, at("2026-11-16T07:00:00Z")).id, 1);
  assert.equal(P.nextShift(shifts, at("2026-11-16T09:00:00Z")).id, 1, "one that is under way is the next");
  assert.equal(P.nextShift(shifts, at("2026-11-16T12:00:00Z")).id, 2, "one that has just ended is not");
  assert.equal(P.nextShift(shifts, at("2026-11-17T13:00:00Z")), null);
  assert.equal(P.nextShift([], 0), null);
  assert.equal(P.nextShift(undefined, 0), null);
});

test("how long ago is whole minutes, never negative", () => {
  assert.equal(P.elapsed(0, 59999), 0);
  assert.equal(P.elapsed(0, 60000), 1);
  assert.equal(P.elapsed(0, 125 * 60000 + 5), 125);
  assert.equal(P.elapsed(5000, 1000), 0, "a clock that runs behind does not give a negative time");
});

test("the rows of the hours report add up", () => {
  const t = P.hoursTotals([
    { planned_minutes: 480, worked_minutes: 500, late: 1, late_minutes: 20, missed: 0, days: 2 },
    { planned_minutes: 240, worked_minutes: 0, late: 0, late_minutes: 0, missed: 1, days: 0 },
  ]);
  assert.deepEqual(t, { planned_minutes: 720, worked_minutes: 500, late: 1, late_minutes: 20, missed: 1, days: 2 });
  assert.deepEqual(P.hoursTotals([]), { planned_minutes: 0, worked_minutes: 0, late: 0, late_minutes: 0, missed: 0, days: 0 });
});

test("the rate in force is the latest that has begun, and the rest are kept as history", () => {
  const rates = [
    { user: "an", from: "2026-12-01", rate: 40000 },
    { user: "an", from: "2026-11-10", rate: 36000 },
    { user: "an", from: "2026-10-01", rate: 30000 },
    { user: "binh", from: "2026-12-05", rate: 28000 },
  ];
  const by = P.ratesOf(rates, "2026-11-14");
  assert.deepEqual(by.map((e) => e.user), ["an", "binh"]);
  assert.equal(by[0].current.rate, 36000, "the one of the 10th, not the one that has not begun");
  assert.deepEqual(by[0].later.map((r) => r.rate), [40000]);
  assert.deepEqual(by[0].earlier.map((r) => r.rate), [30000]);
  assert.equal(by[1].current, null, "a rate that begins later is not in force yet");
  assert.deepEqual(by[1].later.map((r) => r.rate), [28000]);
  assert.equal(P.ratesOf(rates, "2026-12-01")[0].current.rate, 40000, "a rate is in force on its first day");
  assert.deepEqual(P.ratesOf([], "2026-11-14"), []);
});

test("where a person stands with the clock", () => {
  assert.equal(P.clockState(null), "unknown");
  assert.equal(P.clockState({ open: null }), "out");
  assert.equal(P.clockState({ open: { id: 1 } }), "in");
  assert.equal(P.clockState({ open: { id: 1 }, drawer_open: true }), "drawer", "clocked in with a drawer open may not clock out");
  assert.equal(P.clockState({ open: null, drawer_open: true }), "out");
});
