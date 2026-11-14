# 23. Points changed by a manager: one more record in the log, with a reason

Date: 2026-11-14

## Status
Accepted. Adds to [ADR 22](0022-customer-accounts-and-points.md), which left this out.

## Context
In ADR 22 a balance moves only with an order: it is earned when an order is paid, held and spent when points are
used, given back or taken back when an order is cancelled or refunded. That left no way to **give 100 points to
a customer who complained**, to correct a balance that a mistake made, or to start a venue's program by
crediting what a paper card held. The same thing exists for the stock: goods are received and counted, and a
person can adjust a quantity, with a reason.

Two shapes were weighed:

1. **A number a manager may edit** in PostgreSQL or in the state. Then the balance no longer follows from the
   log: it could not be rebuilt, the follower could not know of it, and nobody could say who changed it.
2. **A record in the log**, like the stock movement: *this person gave this customer this many points, for this
   reason*, applied by the state like every other record.

## Decision
- **A change of points is a record of the log** (shape 2), a fourth kind beside the order event, the stock
  movement and the barrier (`Record.Points`): its command ID, the person and their roles, the time, the customer,
  the **signed** number of points, and the reason. The state applies it in order and writes it in the history of
  the customer as `adjusted`, with the reason and the author. The balance stays *derived*:
  `balance = earned - spent + adjusted`.
- **The permission is `points.adjust`, held by the manager alone.** A cashier takes orders and money, and must not
  also make points; the administrator makes accounts and cannot touch orders (ADR 14). A manager **cannot change
  their own points**, so the same hand does not both give and receive.
- **A reason is required** (at most 200 characters), as for a write-off. It is kept with the change, and **the
  customer sees it** on their own screen; the customer does not see **who** made the change, which only staff
  see. A change is of 1 to 10 000 points: the log would hold more (the limit of any balance), but the service keeps
  one change small, so that a mistake or a misuse is small too.
- **What cannot be taken.** Points that orders not paid yet have set aside (held) are not available, so they
  cannot be taken: the change is refused with what could be (`409 points_short`, `max`). A customer with nothing
  cannot be made to owe points; only a refund after the points were spent can do that (ADR 22). Giving points to
  a customer is always allowed, up to the limit of a balance.
- **The same rule for every reader.** The state refuses, and counts, a record that takes more than is available at
  its place in the log, or that would take a balance past the limit: the service checks first, so it happens only
  if something else wrote to the log, as for an order that uses points that are not there. A repeat of a command
  changes nothing.
- **The customer must exist.** The API checks, when there are accounts, that the login is an active customer's
  (the check the counter already has), so that points are not given to a name nobody owns, or to somebody who
  registers it later.
- **The program must be on.** Without `-loyalty-earn-per` and `-loyalty-value` there is nothing to change
  (`409 no_points`).
- **The reports do not count it.** A change is not a sale: the follower of the reports reads the record and counts
  nothing. The points a manager gives are therefore **not a cost in the profit report** until they are spent
  (when they take money off an order, which the report does count as a discount).

## Consequences
- **The audit trail is the log itself**: who, as which role, when, whom, how many, why. It cannot be edited. It
  is also not in the trail of the accounts (that is for accounts), so a person looking for who gave points looks
  at the history of the customer on the desk, which shows the author to staff.
- **Mistakes are corrected by another change** (an equal and opposite one, with a reason), never by erasing.
- **A manager who is dishonest can still give points to a friend's account**, to a size of 10 000 a time and as
  often as they like. The limits make a mistake small; they do not stop abuse. What it has is a trail, that
  nobody can remove it, and that a second manager can read it. A limit per day, or an approval by a second
  person, would be the next step; neither is done.
- **Giving points is a cost that no report shows.** A manager can give points with a value of 1 000 000đ and the
  profit report shows it only when the customer spends them.
- **Not done:** a list of the changes made (the history is per customer), a limit per day, an approval by a
  second person, and a way to give points to many customers at once.
