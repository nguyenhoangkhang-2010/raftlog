-- What things cost. The follower now applies the log with the same rules as the service, lot by lot, so
-- it knows what each use of an ingredient was worth at the price of the lot it came from, and keeps that
-- with the use and with every movement of the stock. It no longer needs what an order was going to take,
-- which it used to keep to work out the use, nor whether the stock was held.
--
-- The facts that were counted before this had no costs, and cannot be given them from what they hold. They
-- are thrown away, with the checkpoint, so that the follower counts the whole log again at the next start.

ALTER TABLE orders DROP COLUMN needs, DROP COLUMN stock_held;
ALTER TABLE ingredient_usage ADD COLUMN value_milli bigint NOT NULL DEFAULT 0;
ALTER TABLE stock_movements ADD COLUMN delta bigint NOT NULL DEFAULT 0, ADD COLUMN value_milli bigint NOT NULL DEFAULT 0;

DELETE FROM stock_movements;
DELETE FROM orders;
DELETE FROM report_checkpoint;
