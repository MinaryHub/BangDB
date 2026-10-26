BEGIN TRANSACTION;
CREATE TABLE kv (k TEXT PRIMARY KEY, v) WITHOUT ROWID;
INSERT INTO "kv" VALUES('x',1.5);
INSERT INTO "kv" VALUES('y','text');
INSERT INTO "kv" VALUES('z',NULL);
CREATE TABLE "order item" (id INTEGER PRIMARY KEY AUTOINCREMENT, order_id INTEGER NOT NULL REFERENCES orders(id) ON DELETE CASCADE, sku VARCHAR(32), qty INT DEFAULT 1, price DECIMAL(10,2), CHECK (qty > 0));
INSERT INTO "order item" VALUES(1,1,'A-1',2,9.5);
INSERT INTO "order item" VALUES(2,1,'B',1,100);
CREATE TABLE orders (id INTEGER PRIMARY KEY, customer TEXT NOT NULL COLLATE NOCASE, created DATETIME DEFAULT CURRENT_TIMESTAMP, note TEXT, data BLOB, "group" TEXT);
INSERT INTO "orders" VALUES(1,'Kim','2026-01-02 03:04:05','it''s; fine',X'00FF','a');
INSERT INTO "orders" VALUES(2,'lee','2026-02-03 00:00:00','multi
line',NULL,NULL);
CREATE INDEX idx_items_order ON "order item"(order_id);
CREATE UNIQUE INDEX idx_orders_cust ON orders(customer, id);
CREATE VIEW totals AS SELECT order_id, SUM(qty*price) AS total FROM "order item" GROUP BY order_id;
CREATE TRIGGER trg AFTER INSERT ON orders BEGIN UPDATE orders SET note = 'new' WHERE id = NEW.id AND note IS NULL; END;
DELETE FROM "sqlite_sequence";
INSERT INTO "sqlite_sequence" VALUES('order item',3);
COMMIT;
