# 19. Reports by following the log

Date: 2026-11-14

## Status
Accepted

## Context
The service answers "what is the state now": the orders in flight, the stock, the cash in a drawer. A manager
asks other questions: what did we sell last week, what sold best, what did the kitchen use, what did we give
back, when are we busy. They are questions over time and over many orders, they change with the manager's
curiosity, and the state of the service is the wrong shape for them (it keeps what is needed to decide the
next command, not what is needed to add up a month). The log has everything, and the log is the one thing
that must not be asked to answer ad hoc queries.

PostgreSQL is already beside the log (ADR 14, 15, 16, 18) and is good at exactly this.

## Decision
- **A follower copies the log into tables; the reports are queries over the copy.** The follower reads
  the records in order from a checkpoint kept in the database, turns each into facts (an order was
  placed, paid, handed over, cancelled or refunded; stock was received, written off, adjusted) and writes a
  **batch of facts and the new checkpoint in one transaction**. A record is counted once, however often the
  follower is stopped and started, or two of them race (the checkpoint row is locked, and a batch that does
  not start where the checkpoint is changes nothing). The log stays the only truth; the tables hold nothing
  that is not in it.
- **Idempotent on every level, because the log can repeat itself.** An event is counted only if it is the next
  of its order (its sequence number follows the last one counted), a placed order and a stock movement are
  inserted `ON CONFLICT DO NOTHING`, so a command retried into the log twice is counted once. A test writes
  every record of an order twice into the log and checks the figures.
- **A copy that can be thrown away.** `-rebuild-reports` deletes the facts and the checkpoint at start and the
  follower reads the log from the beginning; a test compares the figures of a follower that kept up with
  those of one that rebuilt, with another batch size, and they are equal. A bug in the follower is corrected by
  fixing it and rebuilding, never by repairing rows.
- **What an order took from the stock is worked out from what the order carried.** The follower does not know
  the menu. A placed order carries what each line needs, so the follower keeps that with the order and, at
  payment, writes what the order used, and at a cancellation that restocked, writes it back as a negative.
  Stations come from the same event, which is how it knows that an order is ready only when all of them are.
- **A day is a day of the venue.** The queries read each time in the venue's time zone with `AT TIME ZONE`,
  so an order paid at 23:50 in Ho Chi Minh City is on that day and one at 00:10 is on the next, which a
  test checks against UTC. A period is two dates, both included, at most 366 days, and is checked before the
  database is asked. A zone that PostgreSQL does not know (the default, `Local`) turns the reports off with a
  warning instead of giving wrong days.
- **Revenue is money collected on the day, refunds are money given on the day.** An order counts on the day it
  was paid and a refund on the day it was given; net is the one less the other. This is the cash view, not
  the accounting view: an order paid on the 3rd and refunded on the 9th lowers the 9th.
- **Each answer says how old it is.** The reports carry the time of the latest event counted and how many
  records of the log are not yet counted, and the screen shows both, because a copy that lags is honest only
  if it says so.
- **A spreadsheet to download.** Every report is also a CSV, UTF-8 with the byte-order mark (without which
  Excel shows Vietnamese as garbage), amounts as plain whole numbers, and text that begins with `=`, `+`, `-`
  or `@` (a refund reason is typed by a person) with a quote in front, so that a cell can never run as a
  formula. A test puts a formula in a refund reason.
- **Only managers.** `report.view`, which the manager had since the roles were written.

## Consequences
- **The follower trusts the log.** A record that the service skipped because it did not fit (a payment that the
  stock could not cover, written by something that is not this service) is counted by the follower, which does
  not know the rules. The service counts such records and says so in `/healthz`; in normal running there are
  none, because the service checks before it writes.
- **Reports lag by a moment.** The follower waits for the log up to two seconds and writes at once what it
  reads; in a test and on the demo the lag is under a second, and it grows if the database is slow.
- **The copy costs space and a reader.** One more process loop in the service, and rows for every order, line,
  refund, movement of stock and ingredient an order used. It is the price of ad hoc questions; if it grows too
  large the follower can be a program of its own, since it only needs the log and the database.
- **Reading the log for a rebuild takes as long as the log is long.** As with the start of the service, there
  are no snapshots.
- **Prices of goods used are not in the copy.** What a cup costs to make needs the price of the lots the
  ingredients came from, which only the service's state knows. It is the next branch, and will need the follower
  to be told it (for instance by writing it into the log with the order); until then the report of ingredients
  shows quantities, and what was paid for deliveries.
- **Not done:** a report of customers, of promotions, or of stations; comparing two periods; scheduled reports
  by email; and anyone but a manager.
