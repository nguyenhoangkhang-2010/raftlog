# 22. Customers who register, and points: the balance lives in the log

Date: 2026-11-14

## Status
Accepted

## Context
Until now a customer was an account that an administrator made. Two things are asked for: a customer can
sign up by themselves, see their orders, and **earn points that take money off a later order**.

Points are money in all but name, so they must not be lost, doubled or spent twice. The system now keeps its
facts in two places that cannot be changed in one transaction: **PostgreSQL** (accounts, menu, shifts, reports,
...) and the **log** (orders and stock, one writer, the state rebuilt from it). A balance kept in one of them and
decided by an event in the other is the classic dual write. Three shapes were weighed:

1. **Balance in PostgreSQL, updated when an order is paid.** The payment is an event in the log and the balance
   a row in the database. If the service stops between the two, or the database is down at the moment of
   payment, they disagree, and something must find and repair it. A follower can repair *earning* (it reads
   the log and writes once, as the reports do), but not *spending*: a redemption has to be decided against the
   balance at the moment the order is placed, and a follower is always a little behind.
2. **A follower in the middle:** the log holds the orders, PostgreSQL the balance, and spending asks the database
   inside the command. Two stores must then agree before every placement, and a slow or failing database stops
   orders for everyone who uses points.
3. **The balance in the log, as part of the order events.** The service already decides every command on a state
   built from the log, one command at a time. A redemption is checked against that state and written in the
   same record that places the order; an earning is written in the same record that pays it. There is nothing
   to keep in step.

## Decision
- **Points are in the log, and only there** (shape 3). No new kind of record: the event that places an order
  carries the points that were used, and the event that pays it carries the points that were earned. The balance
  of a customer is *derived* from the events by the state, as the stock is, so a restarted or new service
  rebuilds the same balances, and the follower of the reports reads the same facts. PostgreSQL never holds a
  balance, so it can never disagree with one.
- **The rules are in the events, not in the settings.** An event says how many points were used *and what each
  was worth* (`points.used`, `points.value`), and how many were earned. Changing the settings later changes only
  what is decided from then on; replaying the log gives the same balances whatever the settings are now.
- **Earning.** When an order with a customer is paid, it earns `floor(paid / earn_per)` points, where `paid` is
  the money that was paid (after every discount). Both an order from the app and one that the cashier takes for
  a customer at the counter earn, if the cashier names the customer's login; the login is checked against the
  accounts when there is a database, so that points do not go to a name that nobody owns, or to somebody who
  registers that name later.
- **Spending.** Only a customer, on their own order from the app, may use points, from 1 to what is available
  and not more than a share of the order (`max_share`, 50% by default), each worth `value` đồng. They show in
  the order as a discount like the promotions (`ID: "points"`), so the totals, the payment, the cash drawer and
  the reports need no new rule. A command that asks for too many is refused with what the most would be.
- **A hold, then a spend.** The points of an order that is placed and not paid are *held*: they are not
  available for another order, and they are not yet spent. When the order is paid they are spent. When it is
  cancelled unpaid they are released. Two orders placed by one customer cannot both use the same points,
  because the check is made on the state that already holds the first.
- **Cancelling and refunding.** A paid order that is cancelled gives back the points that were used and takes
  back the points that it earned, both in proportion to the money that was given back (all of them for a full
  refund); a partial refund after the
  order was handed over takes back the same share of what it earned, and does not give back points (the money
  came back, the discount was kept). The balance may therefore go below zero for a customer who has spent what an
  order earned and then had the order refunded; it then stays at or below zero, and nothing can be used, until
  new orders earn it back.
- **A record that does not fit is skipped by everyone alike.** An order that uses more points than are available
  at its place in the log, a payment that earns points for an order with no customer, and a placement that
  does not add up are refused by every reader as any other event is, and counted. The service checks first, so
  this happens only if something else wrote to the log.
- **The program is off unless the venue sets it:** `-loyalty-earn-per` (đồng that earn one point), `-loyalty-value`
  (đồng that a point takes off) and `-loyalty-max-share` (percent). With either of the first two at zero no
  points are earned or used, and the screens do not show points.
- **Registration is in PostgreSQL, with the accounts, and is off unless the venue turns it on**
  (`-open-registration`). A person chooses a login, a name and a password, and gets an account with the
  customer role and nothing else (never a role from the request); the password rules are those of every other
  account; the request is limited per address (5 an hour by default), and is written to the trail of accounts
  like every other change. The person is signed in at once.
- **The order carries the login, and the login does not change.** What the log knows of a customer is the login
  name, as it knows it of staff. An account that is disabled keeps its points and cannot sign in; there is no
  deleting an account, so there is no points balance whose owner was deleted.
- **History for the customer.** The customer sees their orders (their own, from the log), and a list of what
  happened to their points: earned, used, given back, taken back, each with the order it came from. The list
  holds the last 200 entries of each customer in memory; the balance does not depend on it.

## Consequences
- **One writer decides the balance, so it is right at every moment**, including after a crash in the middle of a
  command: the points follow the order event that moved them, and that event is either in the log or not.
  What this costs is the same as for the stock: the balances are in the memory of the service and are rebuilt at
  start from the whole log (which grows with every order and every point).
- **A balance cannot be edited.** A mistake is not repaired by changing a number. In this ADR the only ways to
  move points were orders, cancellations and refunds; the manager's *change of points*, a record with a reason
  like a stock adjustment, was added afterwards in [ADR 23](0023-points-changed-by-a-manager.md).
- **Points never expire and have no tiers, no birthday offers, no referral.** The program is a rate and a value.
- **Points are a discount**, so the reports count them as one (net sales are after them, the cost of the
  goods is unchanged) and the profit shows what the program costs. The reports do not yet say how many points
  were earned and used.
- **Registration is not verification.** There is no email or phone, so nobody confirms who they are, and there
  is no way to recover a forgotten password except by an administrator. A person can register many accounts,
  each with its own balance. The limit per address and the password rules slow abuse; a CAPTCHA, a phone number
  or a confirmation by email would be the next steps.
- **Anyone who knows a customer's login can attach their orders at the counter, and earn them points.** That
  helps only the customer; staff cannot spend them.
- **Not done:** expiry, the number of points earned and used in the reports, a
  profile with a phone number, and recovering an account.
