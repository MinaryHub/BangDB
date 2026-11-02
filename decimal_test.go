package bangdb

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestDecimalColumns(t *testing.T) {
	db := open(t)
	exec(t, db, `CREATE TABLE ledger (id INTEGER PRIMARY KEY, account TEXT, amount DECIMAL(12,2) NOT NULL, rate NUMERIC(6,4), f REAL);
		CREATE INDEX ledger_amount ON ledger (amount)`)
	exec(t, db, `INSERT INTO ledger (account, amount, rate, f) VALUES
		('a', 0.1, 0.05, 0.1), ('a', 0.2, '1.23456', 0.2), ('b', '19.99', NULL, NULL), ('b', -5, 2, NULL), ('c', 100.005, NULL, NULL)`)
	expect(t, db, "SELECT amount, typeof(amount), rate FROM ledger ORDER BY id",
		"0.10|decimal|0.0500;0.20|decimal|1.2346;19.99|decimal|NULL;-5.00|decimal|2.0000;100.01|decimal|NULL")
	// Exact arithmetic, unlike REAL.
	expect(t, db, "SELECT sum(amount) = 0.3, sum(amount), sum(f) = 0.3 FROM ledger WHERE account = 'a'", "1|0.30|0")
	expect(t, db, "SELECT account, sum(amount), avg(amount), min(amount), max(amount) FROM ledger GROUP BY account ORDER BY 1",
		"a|0.30|0.15000000|0.10|0.20;b|14.99|7.49500000|-5.00|19.99;c|100.01|100.01000000|100.01|100.01")
	expect(t, db, "SELECT amount * 3, amount + 1, amount / 4, -amount, amount * 1.5, amount % 0.07 FROM ledger WHERE id = 3",
		"59.97|20.99|4.99750000|-19.99|29.985|0.04")
	expect(t, db, "SELECT amount * 0.5e0, typeof(amount * f), amount * f FROM ledger WHERE id = 1", "0.050|decimal|0.010")
	expect(t, db, "SELECT typeof(amount * (1.0 / 3)) FROM ledger WHERE id = 1", "decimal")
	expect(t, db, "SELECT typeof(amount * 1e300) FROM ledger WHERE id = 1", "real") // too large for a decimal
	expect(t, db, "SELECT round(amount / 3, 2), abs(amount), CAST(amount AS INTEGER), CAST('1.005' AS DECIMAL(5,2)) FROM ledger WHERE id = 4",
		"-1.67|5.00|-5|1.01")
	// Comparisons and the index: literals (REAL) match exactly.
	expect(t, db, "SELECT id FROM ledger WHERE amount = 0.1", "1")
	expect(t, db, "SELECT id FROM ledger WHERE amount = 19.990", "3")
	expect(t, db, "SELECT id FROM ledger WHERE amount = 19.991", "") // more digits than the column: no match
	expect(t, db, "SELECT id FROM ledger WHERE amount BETWEEN 0.15 AND 20 ORDER BY amount", "2;3")
	expect(t, db, "SELECT id FROM ledger WHERE amount > -10 ORDER BY amount DESC LIMIT 2", "5;3")
	if plan := query(t, db, "EXPLAIN SELECT id FROM ledger WHERE amount = 0.1"); !strings.Contains(plan, "ledger_amount") {
		t.Errorf("index not used: %s", plan)
	}
	// Range and type checks.
	for q, want := range map[string]string{
		"INSERT INTO ledger (account, amount) VALUES ('x', 12345678901.5)": "does not fit DECIMAL(12,2)",
		"INSERT INTO ledger (account, amount) VALUES ('x', 'abc')":         "cannot store",
		"INSERT INTO ledger (account, amount) VALUES ('x', x'00')":         "cannot store",
	} {
		if _, err := db.Exec(q); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", q, err, want)
		}
	}
	// Go API: Decimal values in and out.
	d, _ := ParseDecimal("1.10")
	exec(t, db, "INSERT INTO ledger (account, amount) VALUES ('go', ?)", d)
	var got Decimal
	var s string
	var f float64
	if err := db.QueryRow("SELECT amount, amount, amount FROM ledger WHERE account = 'go'").Scan(&got, &s, &f); err != nil {
		t.Fatal(err)
	}
	if got.String() != "1.10" || s != "1.10" || f != 1.1 {
		t.Errorf("scan: %v %q %v", got, s, f)
	}
	// JSON and the dump keep every digit.
	expect(t, db, "SELECT json_object('a', amount) FROM ledger WHERE id = 1", `{"a":0.10}`)
	var buf bytes.Buffer
	db.Dump(context.Background(), &buf)
	db2 := open(t)
	if _, err := db2.ImportSQL(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	q := "SELECT id, amount, rate FROM ledger ORDER BY id"
	if a, b := query(t, db, q), query(t, db2, q); a != b {
		t.Errorf("dump round trip:\n%s\n%s", a, b)
	}
}
