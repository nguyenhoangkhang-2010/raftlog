# 20. What things cost: goods from the lots, expenses written down, profit by day

Date: 2026-11-14

## Status
Accepted

## Context
Sales say what came in. A manager asks what is left: what the goods of an order cost, what the venue
spent besides goods, and whether a day made or lost money. Two kinds of cost, with different sources:

- **The goods.** What an order costs in goods is what it took from the stock, at the price of the lots it took
  it from (ADR 16 keeps a price on every lot and uses the lot that expires first). That price is known to the
  service's state when the order is paid, and nowhere else: the log holds the order and the delivery, not the
  lot an order was matched to. The follower of ADR 19 did not have it.
- **The rest.** Rent, power, wages, repairs. They are not in the log and are not events of the kitchen: a
  person writes them down, often for a period, not a day.

## Decision
- **The follower applies the log to a state of its own.** It is the same `State` that the service uses: every
  record goes through it, in order, so the follower makes the decisions the service makes, lot by lot, and sees
  the stock movements that follow, with their worth. It then writes the facts: what an order used (positive when
  paid, negative when a cancellation gave back), with its worth, and each movement of the stock, with how much it
  changed the quantity and what that was worth. Two things follow that were limits of ADR 19: **a record that
  the service skipped is skipped here too** (a payment the stock could not cover is not a sale), and
  **the cost of an order is the cost of the lots that were used**, with the lot that expires first used first and
  an expired lot never. The price is a second copy of the state in memory, and a replay of the log at start of
  the follower, which the service does as well; a follower that restarts rebuilds its state to the checkpoint
  without writing, then goes on.
- **Costs are kept in thousandths of a đồng, and rounded once.** A gram of coffee costs a fraction of a đồng.
  The facts keep the exact worth; a day's cost is rounded to whole đồng when it is shown, and the rows of a
  report add up to its total as they are shown.
- **Waste is what left the stock and was not sold.** A write-off and a stocktake that found less than there
  should be, at the price of the lots they came from, on the day they were written. A surplus is not a gain: it
  is found goods and enters the stock at an average price, but it earns nothing until it is sold.
- **Expenses are written down, never changed.** A manager writes a category, an amount and the day or days it
  covers; a mistake is voided, with a reason, and written again. The database refuses to change or delete one
  (a trigger), as it does for the trail of accounts and the shifts. They live in their own tables and are not
  touched by a rebuild of the reports: they are not copies of anything.
- **A period is spread evenly, in whole đồng.** The rent of a month is a thirtieth of itself each day, the
  remainder going to the first days, so that the days of an expense add up to its amount exactly (a test checks
  ten million over thirty days and five đồng over ten).
- **Profit by day is net sales, less goods used, less waste, less the day's share of the expenses.** Net sales
  are what the reports of ADR 19 call net: collected on the day less given back on the day. A day with no sales
  still carries its rent, and is a loss.
- **What an item costs to make** is its base recipe at the price each ingredient cost the last time it was
  bought, with the ingredients that have no price listed and the item flagged as costing more than shown. It is
  the cost *now*, for pricing; the profit report is the cost *as it was*, from the lots.
- **Who.** Profit and item costs need `report.view` and `stock.view_cost`; expenses need `expense.manage`. The
  manager has all three; the storekeeper may see what things cost and does not read the reports.
- **A migration that throws the copy away.** The reports' tables did not carry costs and cannot be given them
  from what they hold, so migration 2 deletes the facts and the checkpoint, and the follower counts the whole
  log again at the next start. The tables of expenses are in a set of their own and are not touched.

## Consequences
- **The follower is as heavy as the service.** Two states in one process, and two replays at start. If that
  hurts, the follower can be a program of its own, and the service's state can be shared by an interface; neither
  is needed yet.
- **The profit is only as good as the prices on the deliveries.** An ingredient that came with no price counts
  for nothing, so the cost of goods is what is known and the profit is more than it is. The screens say which items
  have an ingredient with no price, and a delivery says nothing when its price is left empty.
- **The cost of an item sold is not known per order line.** The follower knows what an order used, by ingredient,
  not what each line of it used, so the report of best sellers has revenue and no cost, and the margin of an item
  is theoretical (its recipe at the latest price), not what the orders that sold it cost.
- **Options and combos have no cost of their own.** An item is its base recipe; an option that adds an
  ingredient costs what it costs inside the orders, and shows in the profit by day, not in the item.
- **Cash accounting for money, not accrual for everything.** Sales count on the day they are paid and refunds on
  the day they are given; goods on the day they are used, not bought; expenses on the days they cover, not the
  day they were written. A delivery bought on the 1st and used over three weeks costs the three weeks.
- **Not done:** wages by shift (done in [ADR 21](0021-the-schedule-and-the-time-clock.md), from the time clock), taxes, depreciation of
  equipment, a budget, and the comparison of two periods.
