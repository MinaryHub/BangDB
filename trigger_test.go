package bangdb

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestTriggers(t *testing.T) {
	db := open(t)
	exec(t, db, `
		CREATE TABLE acct (id INTEGER PRIMARY KEY, owner TEXT, balance DECIMAL(10,2) NOT NULL, updated INT DEFAULT 0);
		CREATE TABLE audit (id INTEGER PRIMARY KEY AUTOINCREMENT, what TEXT, acct INT, old DECIMAL(10,2), new DECIMAL(10,2));
		CREATE TRIGGER acct_ins AFTER INSERT ON acct BEGIN
			INSERT INTO audit (what, acct, new) VALUES ('insert', NEW.id, NEW.balance);
		END;
		CREATE TRIGGER acct_upd AFTER UPDATE OF balance ON acct FOR EACH ROW WHEN NEW.balance <> OLD.balance BEGIN
			INSERT INTO audit (what, acct, old, new) VALUES ('update', NEW.id, OLD.balance, NEW.balance);
			UPDATE acct SET updated = updated + 1 WHERE id = NEW.id;
		END;
		CREATE TRIGGER acct_del BEFORE DELETE ON acct BEGIN
			SELECT RAISE(ABORT, 'cannot delete an account with money') WHERE OLD.balance > 0;
			INSERT INTO audit (what, acct, old) VALUES ('delete', OLD.id, OLD.balance);
		END;
		CREATE TRIGGER no_negative BEFORE INSERT ON acct WHEN NEW.balance < 0 BEGIN
			SELECT RAISE(IGNORE);
		END;`)
	exec(t, db, "INSERT INTO acct (id, owner, balance) VALUES (1, 'kim', 10), (2, 'lee', 0), (3, 'bad', -5)")
	expect(t, db, "SELECT id FROM acct ORDER BY id", "1;2") // RAISE(IGNORE) skipped the negative row
	exec(t, db, "UPDATE acct SET balance = balance + 5 WHERE id = 1")
	exec(t, db, "UPDATE acct SET owner = 'KIM' WHERE id = 1")      // not OF balance: no trigger
	exec(t, db, "UPDATE acct SET balance = balance WHERE id = 2") // WHEN is false
	expect(t, db, "SELECT updated FROM acct ORDER BY id", "1;0")
	if _, err := db.Exec("DELETE FROM acct WHERE id = 1"); err == nil || !strings.Contains(err.Error(), "cannot delete an account with money") {
		t.Fatalf("RAISE(ABORT): %v", err)
	}
	expect(t, db, "SELECT count(*) FROM acct", "2") // the failed statement changed nothing
	exec(t, db, "DELETE FROM acct WHERE id = 2")
	expect(t, db, "SELECT what, acct, old, new FROM audit ORDER BY id",
		"insert|1|NULL|10.00;insert|2|NULL|0.00;update|1|10.00|15.00;delete|2|0.00|NULL")
	// UPSERT's update fires UPDATE triggers.
	exec(t, db, "INSERT INTO acct (id, owner, balance) VALUES (1, 'x', 20) ON CONFLICT (id) DO UPDATE SET balance = excluded.balance")
	expect(t, db, "SELECT count(*) FROM audit WHERE what = 'update'", "2")

	// Catalog: persistence through dumps, DROP, errors.
	var buf bytes.Buffer
	db.Dump(context.Background(), &buf)
	if !strings.Contains(buf.String(), "CREATE TRIGGER acct_upd AFTER UPDATE OF balance ON acct") {
		t.Fatalf("dump lacks triggers:\n%s", buf.String())
	}
	db2 := open(t)
	if _, err := db2.ImportSQL(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	exec(t, db2, "INSERT INTO acct (id, owner, balance) VALUES (9, 'z', 1)")
	expect(t, db2, "SELECT what FROM audit WHERE acct = 9", "insert")
	exec(t, db, "DROP TRIGGER acct_ins")
	exec(t, db, "INSERT INTO acct (id, owner, balance) VALUES (4, 'p', 1)")
	expect(t, db, "SELECT count(*) FROM audit WHERE acct = 4", "0")
	for q, want := range map[string]string{
		"CREATE TRIGGER acct_upd AFTER INSERT ON acct BEGIN SELECT 1; END":     "already exists",
		"CREATE TRIGGER x AFTER INSERT ON nope BEGIN SELECT 1; END":            "no such table",
		"CREATE TRIGGER x INSTEAD OF INSERT ON acct BEGIN SELECT 1; END":       "not supported",
		"CREATE TRIGGER x AFTER INSERT ON acct BEGIN CREATE TABLE y (a); END":  "trigger body",
		"CREATE TRIGGER x AFTER UPDATE OF nope ON acct BEGIN SELECT 1; END":    "no such column",
		"DROP TRIGGER nope":                                                    "no such trigger",
		"ALTER TABLE acct RENAME TO accounts":                                  "has triggers",
		"SELECT RAISE(ABORT)":                                                  "needs a message",
	} {
		if _, err := db.Exec(q); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", q, err, want)
		}
	}
	// A trigger that fires itself stops at the recursion limit.
	exec(t, db, `CREATE TABLE loop (n INT); CREATE TRIGGER again AFTER INSERT ON loop BEGIN INSERT INTO loop VALUES (NEW.n + 1); END`)
	if _, err := db.Exec("INSERT INTO loop VALUES (1)"); err == nil || !strings.Contains(err.Error(), "recursion") {
		t.Fatalf("recursive trigger: %v", err)
	}
	expect(t, db, "SELECT count(*) FROM loop", "0")
	exec(t, db, "DROP TABLE loop")
	if names := db.Engine().Catalog().TriggerNames(); strings.Join(names, ",") != "acct_del,acct_upd,no_negative" {
		t.Fatalf("triggers after DROP TABLE: %v", names)
	}
}
