# 16. The warehouse: lots in the log, suppliers and stocktakes in PostgreSQL

Date: 2026-11-14

## Status
Accepted

## Context
The stock was one number for each ingredient, changed by four kinds of movement. That is enough to
say "sold out", and not enough to run a warehouse: it cannot say that the milk that came last Tuesday
expires before the milk that came on Friday, so it cannot use the older first or warn that something
is about to be thrown away; it does not know what anything cost, so nothing later can say what a
cup of coffee costs to make (the branch after the reports needs that); and a count of the shelves
is one number typed at one moment, while a real count takes an hour, during which the shop sells.

Three things pull in different directions. Quantities decide whether an order can be paid, so they
must be exactly the same on every reader and cannot wait for a second system: they are in the log
with the orders. Who the goods come from, what an ingredient is called, and a count in progress are
documents that people write and change: they are rows. And what a screen shows (a history, a waste
report) is a view that must not become a second source of truth.

## Decision
- **Stock is kept in lots, in the log.** A delivery makes a lot of an ingredient with the date after
  which it must not be used (or none), what was paid for all of it, the supplier's ID and the number of
  the document. The quantity of an ingredient is always the sum of its lots, and a test checks that
  after every movement of a long random sequence. A movement is still applied by one function, and the
  same movements in the same order give the same lots on every machine; a test replays them to prove it.
  Logs written before this change are read as they are: a receipt without a date or a price is a lot
  with neither.
- **The lot that expires first is used first** (a lot with no date after those that have one, among
  equals the oldest). **A lot that has expired by the time an order is paid is not used for it**, and it
  is not counted when the menu decides whether an item is sold out; it stays in the stock, flagged, until
  somebody writes it off. The state has no clock, so the decision uses the time written in the event of
  the payment: every reader, now or after a restart, decides alike. The first version of this rule would
  have let expired milk go into a cup, because it takes the lot that expires first; the test that makes an
  order after a lot has expired is there for that.
- **An order gives back to the lot it took from.** What a payment took (lot, quantity, the lot's date and
  price) is kept for the order until it is handed over or cancelled; a cancellation before the kitchen
  starts puts it back in the same lot, remade with its date and price if the lot had been used up. Waste
  and the cost of goods sold would be wrong otherwise.
- **Prices are in thousandths of a đồng.** A gram of coffee costs a fraction of a đồng, so a lot keeps
  `total paid * 1000 / quantity`, rounded, and values are quantity times that. A line with no price counts
  for nothing and the screen says how much of the quantity has none; an average price is invented only for
  a surplus that a stocktake finds (it takes the average of what is in stock, which it says nowhere is a
  purchase).
- **Two new kinds of movement.** `adjust` is a signed difference with a reason, which a stocktake writes;
  a write-off may name the lot it comes from and a cause (spoiled, expired, spilled, other), which the waste
  report adds up. `count` (set the quantity to what was found) still exists and is now an adjustment by the
  difference.
- **Deliveries have lines and are written one line at a time.** A delivery is one command with its lines
  under the IDs `<command>-01`, `-02`, ...; each line is a record of its own. All lines are tried on a copy
  of the stock before the first is written, and a delivery that was cut short is finished by sending the same
  command again: the lines that are in the log are not written twice.
- **Suppliers, names and units, and stocktakes are in PostgreSQL**, in a third set of migrations with
  its own history table, next to accounts and the catalog. A supplier is never deleted, only made inactive,
  so an ID in the log never points at nothing, and a delivery names only suppliers that are in use.
- **A stocktake is counted over time and applied once.** Each line says what was found and what the stock
  said at that moment; applying it writes, for each ingredient that differs, the difference between the two
  (not "set to what was found"), so what was sold while the shelves were counted is not taken off twice.
  The states are open, applying, applied and cancelled; only one stocktake is open at a time (a partial
  unique index, so two people starting at once get one and a refusal); once applying has begun nothing can
  change; the adjustments have fixed IDs, so an apply that was cut short is finished by pressing the same
  button. A loss larger than what is there now is written as what is there, never below zero.
- **The history is a view of the log**, kept in memory for each ingredient (the latest 1000 movements) and
  rebuilt at every start like the orders. The waste report and the list of deliveries are read from it. The
  screens say when older movements were dropped.
- **Who may.** Looking at the warehouse is `stock.view` (kitchen, manager, storekeeper); what things cost
  is `stock.view_cost` (manager, storekeeper), so a cook sees quantities and dates and no prices; receiving is
  the storekeeper's (`stock.receive`); write-offs, adjustments and stocktakes are the storekeeper's and the
  manager's (`stock.adjust`); suppliers and the names and units of ingredients are `warehouse.manage`
  (storekeeper, manager).
- **A flag, `-expiry-warning`** (72 hours by default), says how long before it expires a lot is flagged.

## Consequences
- **The history is not the audit.** It stops at 1000 movements for an ingredient that is used all day (the
  log has them all), so a waste report over a long period of a busy ingredient is incomplete, and says so. The
  proper reports are the next branches, which will follow the log into PostgreSQL.
- **A rebuild is slower.** Every paid order records the lots it used and every stock movement an entry of the
  history; the memory for the history is bounded, the time to rebuild still grows with the log, as before.
- **The choice of lot is the program's, not the cook's.** A cook who takes the second shelf first is not
  followed: the system assumes the oldest goes first. Writing off from a named lot exists; using from one does
  not.
- **Expiry is a date in the venue's time zone.** "The last day it may be used" ends at the start of the next day
  in that zone; a lot with no date never expires. The time of day an ingredient goes off is not modelled.
- **Cost comes from the price written on the delivery**, not from what the invoice says later: a correction of a
  price is an adjustment of the stock, not of the lot. A delivery cannot be edited, only compensated.
- **Two systems again, no shared transaction.** A stocktake in PostgreSQL and its adjustments in the log can
  disagree for as long as an apply takes, and after a crash until it is pressed again; its state says
  "applying", which is shown. A supplier made inactive after a delivery does not change the delivery.
- **Not done:** units that convert (a recipe in grams against a delivery in kilograms), reorder points and
  purchase orders, transfers between places, lots chosen by hand for an order, a value of the stock at a past
  date, and the correction of a delivery.
