# 18. Shifts and the cash drawer: counted in PostgreSQL, reconciled against the log

Date: 2026-11-14

## Status
Accepted

## Context
Cash is the one kind of payment that leaves the system: the service knows that a customer handed over
100,000 for a 58,000 order and that 42,000 went back, and that is all it can know. Whether the drawer holds
the right money at the end of the day is a fact about notes in a box, and the only way to learn it is to
count. A shop that never counts does not know whether it is losing money, and a cashier who counts knowing
what the answer should be will make the count agree.

The two halves live in two places. What the orders brought in is in the log, written by the system as it
happened and not by the person whose drawer it is. What was counted is typed by a person, in a form, at a
moment; it is a document, like a stocktake, and belongs with the accounts, the menu and the suppliers.

## Decision
- **A shift is a row in PostgreSQL.** It is opened by a cashier at a named drawer with a count (how many notes
  and coins of each denomination, 500,000 down to 200), and closed with another. The count is stored as the
  counts and as the total that the **service** worked out from them, never a total sent by the screen. A
  cashier has one open shift, and a drawer is in one open shift (two partial unique indexes, so two people
  starting at once get one and a refusal, tested).
- **What the drawer should hold is worked out at the close, from the log.** Expected = opening count + cash
  sales - cash refunds + money put in - money taken out, and the variance is the count less that, negative when
  the drawer is short. The cash sales and refunds come from the service, which keeps for every cashier the
  cash that orders brought in and gave back (built from the log like the orders, and the same after a restart,
  tested); the money put in and out are rows the cashier wrote during the shift. All five figures are stored
  with the shift, so a shift says later why it was short or over, and does not have to be recomputed from a log
  that may have grown.
- **The cashier counts blind.** The screen does not show what the drawer should hold until the shift is
  closed; a manager sees it while the shift is open. A count made while knowing the answer is a copy of the
  answer. After the close the cashier sees everything.
- **Cash belongs to the cashier who took it.** A payment in cash is counted for the cashier whose name is on
  the payment and falls in the window of their shift. A refund of cash is counted against the cashier who took
  the cash in the first place, because that drawer has the money, and only if it falls in that cashier's
  shift. A refund of a card payment is not cash. This is simple and deterministic, and it is also the place
  where reality will differ (see below).
- **A closed shift is not rewritten.** The database refuses to update any column of a closed shift except the
  review, refuses to review one twice, and refuses deletes and truncates; the movements are append-only. A
  test tries each. The guard is a trigger, so it protects against a bug and a careless statement, not against
  the owner of the database (the same limit as the trail of accounts).
- **A manager reviews a shift that is off.** The review is one note by one person, once. A manager may also
  close a shift that a cashier left open, with a count of their own and the closing marked as theirs.
- **Requiring a shift is a flag.** With `-require-shift` a payment in cash is refused (409, `no_shift`) unless
  the cashier has a shift open; card and transfer are never refused, since they do not touch a drawer. It is
  off by default because a venue that has not started counting would otherwise be unable to sell, and it
  needs the database because the shifts are in it.
- **Money in and out is by the hand that has the drawer.** Only the cashier of a shift records money put in or
  taken out of it, with a reason; a manager cannot record it for them.

## Consequences
- **Attribution is by name and by time.** A drawer shared by two cashiers at once is not modelled: each opens
  a shift, each has a drawer name, and their cash is separate. Two people serving from one drawer would
  both want their cash counted in it.
- **A refund after the shift is closed is in no drawer.** If a manager approves a cash refund the next
  morning, the cashier's shift is over and the refund is not in it; the money came out of something, and the
  system does not know what. The movement "money taken out" has to be recorded in a shift that is open.
- **There is a small window at the close.** The figures are read from the service at the moment of closing;
  a payment that is being written at that instant, with a time before it, may not be in the state yet and
  would then be in no shift. The window is the length of one write to the log.
- **The clock is the machine's.** The shift's times and the times of the payments come from the same
  machine, so the window means what it says; a service and a database on machines with different clocks
  would shift the window by the difference.
- **Notes only.** The count is by denomination; foreign money, vouchers and tips are not modelled, and a
  coin of a kind that is not in the list cannot be counted.
- **Not done:** a safe or a bank deposit as a place of its own, a float that carries from one shift to the
  next (each opening is counted afresh), a report of variance by cashier over time (the next branches read
  the same rows), and a rule that a variance above a limit blocks the cashier from the next shift.
