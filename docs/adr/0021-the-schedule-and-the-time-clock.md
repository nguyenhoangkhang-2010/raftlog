# 21. The schedule, the time clock and what labour costs

Date: 2026-11-14

## Status
Accepted

## Context
A venue with staff needs to say who works when, and to know when they did. Three questions follow, and
each has a different answer for how it is kept:

- **Who is on, and when?** A plan, written ahead by a manager, changed and called off.
- **Who came, and for how long?** A record that a person makes by clocking in and out, and that a manager
  must sometimes correct (a phone left at home, a door that did not open).
- **What did it cost?** Wages are the largest cost of most venues, and ADR 20 left them out ("wages by shift
  could be read from the schedule").

None of this is an order or a movement of stock. The log of the kitchen is a single writer's sequence of what
happened to orders and goods, from which the service's state is rebuilt; a plan of shifts and a time sheet are
not part of that state, and nothing in the kitchen's decisions depends on them.

## Decision
- **The staff data is in PostgreSQL, in a set of tables of its own** (`rota`, `time_entries`,
  `time_entry_history`, `hourly_rates`, with their own migrations). It is not written to the log: no order
  is decided by it, and putting it there would only make the log longer and the rebuild slower. What must be
  right under two people acting at once is made right by the database: a person has at most one open entry
  (a partial unique index), and the shifts and entries of a person are changed under a lock taken for that
  person (`pg_advisory_xact_lock`), so two checks for an overlap cannot both pass (a test sends eight at once
  and exactly one wins).
- **A day is a day of the venue.** The store is made with the venue's time zone and works out the local date in
  Go; the tables keep the moments (`timestamptz`) and the local day on which each began, so that a report by
  day needs no zone in SQL. A shift whose end is not after its start runs into the next day, and belongs to the
  day it began on; an entry that crosses midnight also belongs to the day it began.
- **The schedule.** A shift has a person, a day, a start and an end, a station and a note; it is 30 minutes to
  16 hours long and may not overlap another planned shift of the same person (touching is fine). It is changed
  by a manager, or cancelled with a reason, and then stays on record, dimmed, and cannot be changed (a trigger).
  A manager can copy a week to the next; a copy that would overlap what is there is left out and counted.
  Only a person who works shifts (a role that holds `clock.use`) can be given one, and not a disabled account.
- **The clock.** Clocking in opens an entry; clocking out closes it. The entry is matched to the planned shift
  of the person that is nearest to now, if it starts within three hours or is under way; the match is only a
  note. A person who has no shift can still clock in. An entry lasts at most 24 hours; a click that would make
  it longer is refused and a manager corrects it.
- **Corrections are written, not edited.** A manager can write down work that was not clocked, or correct an
  entry, always with a reason; what the entry was is kept in an append-only history with who changed it, and the
  database refuses to delete an entry, to open a closed one, or to give one to someone else. An entry that a
  manager wrote is marked so, and so is one that was corrected.
- **Hours compare the plan with what happened.** Per person and period: the length of the shifts planned, the
  time clocked, the days worked, the shifts that began more than five minutes late (and by how much) and the
  shifts that are over with nobody there. *Attendance is read from overlap, not from the match*: an entry that
  overlaps a shift counts as having come, so work that a manager wrote down for someone who forgot to clock is
  not a missed shift, and a person who was in at the start of a shift is not late because they had not clocked
  out of the one before.
- **Rates are written, never changed.** An hourly rate is a whole number of đồng from a day on; the one in force
  on a day is the latest that starts on or before it, the one written last if two start the same day. Rates are
  seen and set by those who hold `staff.manage` (the manager).
- **Labour is in the profit report.** Each day carries the minutes that were worked on it, whole minutes of
  closed entries, times the rate in force that day, divided by 60 and rounded once for the day (the rows of the
  report add up to its total as shown). An entry still open is not counted. Minutes of someone who has no rate
  cost nothing, and the report says how many there were, so that a profit that is more than it should be says
  so. Wages are *not* also written as expenses; the screen of rates says so.
- **Two links to the cash drawer.** A person with a cash drawer open in their name may not clock out
  (`409 drawer_open`): a drawer left open is a count that nobody owns. And with `-require-clock-in` (off by
  default) a cash shift can only be opened by someone who is clocked in (`409 clock_in_first`), as a counterpart
  of `-require-shift`.
- **Who.** `clock.use` goes to the roles that work shifts (cashier, kitchen, runner, storekeeper, manager);
  `staff.manage` only to the manager. A person who clocks sees and clocks themself only, the manager everyone.
  The customer and the administrator do not clock in.

## Consequences
- **Nothing here checks who is holding the phone.** A person can clock in for a friend; there is no place, no
  network, no photo. What the system gives is a trail: every entry says who made it, and a correction says who
  and why. Whether that is enough is for the venue to say; stronger checks (a shared tablet at the door, a PIN)
  are the next step if it is not.
- **Hours are minutes, not pay.** There are no breaks, no overtime or night rates, no leave, holidays or
  sickness, no payroll, tax or insurance. A rate is one number for an hour. The labour in the report is what the
  clock times the rate says, not what is paid.
- **A rate can be written for the past.** A manager who writes a rate from an earlier day changes what the
  profit of the days since says. The history shows who wrote it and when, and nothing is lost, but the report of
  a closed month can move; the same holds for correcting an old entry. This is deliberate (a mistake in a rate
  should be fixable) and is the manager's to use with care.
- **"Planned" includes what has not happened yet.** The difference between planned and worked in a period that
  includes today looks worse than it is until the day is over.
- **The day is fixed when it is written.** The local day of a shift and of an entry is stored when it is made;
  changing the venue's time zone later does not move the old ones.
- **The matching of a clock-in to a shift is a heuristic.** Three hours before and until the end, the nearest
  start. A person with two shifts close together may be matched to the wrong one; attendance does not depend on
  it, but the link shown on an entry may.
- **Not done:** breaks and overtime, leave and swaps of shifts between people, a view for staff to ask for a
  change, notifications, labour by station or by item, and payroll.
