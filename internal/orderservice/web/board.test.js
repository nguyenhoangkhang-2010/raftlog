"use strict";
// Tests of what the call board works out for itself: which numbers have just become ready (so that
// the screen can call them), how long an order has waited, and when the screen is no longer to be
// trusted. Run with: node --test internal/orderservice/web/
const test = require("node:test");
const assert = require("node:assert/strict");
const board = require("./board.js");

test("a number is called when it was not ready on the last reading and is now", () => {
  const before = [{ number: 4 }, { number: 6 }];
  const now = [{ number: 4 }, { number: 6 }, { number: 9 }, { number: 10 }];
  assert.deepEqual(board.newlyReady(before, now), [9, 10]);
  assert.deepEqual(board.newlyReady(now, before), [], "a number that left is not called");
  assert.deepEqual(board.newlyReady(before, before), []);
  assert.deepEqual(board.newlyReady([], [{ number: 1 }]), [1], "from an empty board, everything that appears is new");
});

test("a screen that has just been switched on calls nothing", () => {
  assert.deepEqual(board.newlyReady(null, [{ number: 3 }, { number: 4 }]), []);
});

test("minutes waited are whole minutes and never below zero", () => {
  const now = Date.parse("2026-11-20T10:10:30Z");
  assert.equal(board.minutesSince("2026-11-20T10:10:00Z", now), 0);
  assert.equal(board.minutesSince("2026-11-20T10:09:00Z", now), 1);
  assert.equal(board.minutesSince("2026-11-20T10:00:00Z", now), 10);
  assert.equal(board.minutesSince("2026-11-20T10:30:00Z", now), 0, "a clock that is ahead does not give a negative wait");
  assert.equal(board.minutesSince("not a time", now), 0);
  assert.equal(board.minutesSince("", now), 0);
});

test("the board is stale when it was never read, or not for longer than the limit", () => {
  assert.equal(board.isStale(0, 1000, 60000), true);
  assert.equal(board.isStale(1000, 30000, 60000), false);
  assert.equal(board.isStale(1000, 61000, 60000), false, "exactly at the limit is still fresh");
  assert.equal(board.isStale(1000, 61001, 60000), true);
});
