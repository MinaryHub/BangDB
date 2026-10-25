package bangdb

import (
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/knoxxr/bangdb/internal/value"
)

func open(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		checkIntegrity(t, db)
		db.Close()
	})
	return db
}

// checkIntegrity fails the test if the database has any structural problem.
func checkIntegrity(t *testing.T, db *DB) {
	t.Helper()
	problems, err := db.IntegrityCheck()
	if err != nil {
		t.Errorf("integrity check: %v", err)
	}
	for _, p := range problems {
		t.Errorf("integrity: %s", p)
	}
}

func exec(t *testing.T, db *DB, q string, args ...any) Result {
	t.Helper()
	r, err := db.Exec(q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return r
}

// query returns rows rendered as "a|b|c" lines joined by ";".
func query(t *testing.T, db *DB, q string, args ...any) string {
	t.Helper()
	rows, err := db.Query(q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return render(rows)
}

func render(rows *Rows) string {
	var lines []string
	for rows.Next() {
		var parts []string
		for _, v := range rows.Values() {
			parts = append(parts, value.Format(v))
		}
		lines = append(lines, strings.Join(parts, "|"))
	}
	return strings.Join(lines, ";")
}

func expect(t *testing.T, db *DB, q, want string, args ...any) {
	t.Helper()
	if got := query(t, db, q, args...); got != want {
		t.Errorf("%s\n got: %s\nwant: %s", q, got, want)
	}
}

func setupShop(t *testing.T) *DB {
	db := open(t)
	exec(t, db, `
		CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			email TEXT UNIQUE,
			age INT DEFAULT 30
		);
		CREATE TABLE orders (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER,
			item TEXT,
			amount REAL
		);
		CREATE INDEX idx_orders_user ON orders (user_id);
		INSERT INTO users (name, email, age) VALUES
			('kim', 'kim@x.io', 31), ('lee', 'lee@x.io', 25), ('park', NULL, 40), ('choi', 'choi@x.io', NULL);
		INSERT INTO orders (user_id, item, amount) VALUES
			(1, 'book', 12.5), (1, 'pen', 2), (2, 'book', 12.5), (3, 'laptop', 1200), (9, 'ghost', 1);
	`)
	return db
}

func TestSelectBasics(t *testing.T) {
	db := setupShop(t)
	expect(t, db, `SELECT id, name FROM users WHERE age > 30 ORDER BY id`, "1|kim;3|park")
	expect(t, db, `SELECT name FROM users WHERE age IS NULL`, "choi")
	expect(t, db, `SELECT name FROM users WHERE email IS NULL OR age < 26 ORDER BY name`, "lee;park")
	expect(t, db, `SELECT name FROM users ORDER BY age DESC, name LIMIT 2`, "park;kim")
	expect(t, db, `SELECT name FROM users ORDER BY id LIMIT 2 OFFSET 1`, "lee;park")
	expect(t, db, `SELECT name FROM users ORDER BY id LIMIT 1, 2`, "lee;park")
	expect(t, db, `SELECT name FROM users WHERE name LIKE 'K%' OR name LIKE '_ee'`, "kim;lee")
	expect(t, db, `SELECT name FROM users WHERE id IN (2, 4, 99) ORDER BY id`, "lee;choi")
	expect(t, db, `SELECT name FROM users WHERE id NOT IN (2, 4)`, "kim;park")
	expect(t, db, `SELECT name FROM users WHERE age BETWEEN 25 AND 31 ORDER BY age`, "lee;kim")
	expect(t, db, `SELECT DISTINCT item FROM orders ORDER BY item`, "book;ghost;laptop;pen")
	expect(t, db, `SELECT u.name AS n FROM users AS u WHERE u.rowid = 2`, "lee")
	expect(t, db, `SELECT * FROM users WHERE id = 1`, "1|kim|kim@x.io|31")
	expect(t, db, `SELECT name, age * 2 + 1 FROM users WHERE id = ?`, "lee|51", 2)
	expect(t, db, `SELECT CASE WHEN age >= 30 THEN 'senior' WHEN age IS NULL THEN '?' ELSE 'junior' END FROM users ORDER BY id`,
		"senior;junior;senior;?")
	expect(t, db, `SELECT UPPER(name), LENGTH(name), SUBSTR(name, 2, 2), COALESCE(email, '-') FROM users WHERE id = 3`, "PARK|4|ar|-")
	expect(t, db, `SELECT 1 + 2, 7 / 2, 7.0 / 2, 7 % 3, 'a' || 1, 1 / 0, NULL = NULL, NULL IS NULL`, "3|3|3.5|1|a1|NULL|NULL|1")
	expect(t, db, `SELECT ROUND(2.567, 2), ABS(-3), TYPEOF(1.5), CAST('42' AS INTEGER) + 1, MAX(1, 5, 3)`, "2.57|3|real|43|5")
	expect(t, db, `SELECT name FROM users ORDER BY 1 DESC LIMIT 1`, "park")
}

func TestAggregatesAndGroupBy(t *testing.T) {
	db := setupShop(t)
	expect(t, db, `SELECT COUNT(*), COUNT(email), SUM(age), MIN(age), MAX(name), AVG(age) FROM users`, "4|3|96|25|park|32")
	expect(t, db, `SELECT COUNT(*), SUM(age), MAX(age) FROM users WHERE id > 100`, "0|NULL|NULL")
	expect(t, db, `SELECT item, COUNT(*) AS n, SUM(amount) FROM orders GROUP BY item HAVING n > 1 OR SUM(amount) > 1000 ORDER BY item`,
		"book|2|25;laptop|1|1200")
	expect(t, db, `SELECT user_id, COUNT(DISTINCT item) FROM orders GROUP BY user_id ORDER BY COUNT(*) DESC, user_id LIMIT 2`, "1|2;2|1")
	expect(t, db, `SELECT GROUP_CONCAT(name) FROM users WHERE age > 30`, "kim,park")
	expect(t, db, `SELECT COUNT(*) FROM orders GROUP BY user_id HAVING COUNT(*) > 5`, "")
	if _, err := db.Query(`SELECT name FROM users WHERE COUNT(*) > 1`); err == nil {
		t.Error("aggregate in WHERE must fail")
	}
}

func TestJoins(t *testing.T) {
	db := setupShop(t)
	expect(t, db, `SELECT u.name, o.item FROM users u JOIN orders o ON o.user_id = u.id ORDER BY o.id`,
		"kim|book;kim|pen;lee|book;park|laptop")
	expect(t, db, `SELECT u.name, o.item FROM users u LEFT JOIN orders o ON o.user_id = u.id WHERE u.id >= 3 ORDER BY u.id`,
		"park|laptop;choi|NULL")
	expect(t, db, `SELECT u.name FROM users u LEFT JOIN orders o ON o.user_id = u.id WHERE o.id IS NULL`, "choi")
	expect(t, db, `SELECT u.name, SUM(o.amount) AS total FROM users u, orders o WHERE u.id = o.user_id GROUP BY u.name ORDER BY total DESC`,
		"park|1200;kim|14.5;lee|12.5")
	expect(t, db, `SELECT a.name, b.name FROM users a JOIN users b ON a.age < b.age WHERE a.id = 2 ORDER BY b.age`,
		"lee|kim;lee|park")
	expect(t, db, `SELECT o.item, u.name FROM orders o LEFT JOIN users u ON u.id = o.user_id AND u.age > 30 ORDER BY o.id`,
		"book|kim;pen|kim;book|NULL;laptop|park;ghost|NULL")
	expect(t, db, `SELECT users.*, orders.item FROM users JOIN orders ON orders.user_id = users.id WHERE orders.amount > 100`,
		"3|park|NULL|40|laptop")
	if _, err := db.Query(`SELECT id FROM users JOIN orders ON orders.user_id = users.id`); err == nil {
		t.Error("expected ambiguous column error")
	}
}

func TestExplainUsesIndexes(t *testing.T) {
	db := setupShop(t)
	plan := func(q string) string {
		rows, err := db.Query("EXPLAIN " + q)
		if err != nil {
			t.Fatal(err)
		}
		return render(rows)
	}
	cases := map[string]string{
		`SELECT * FROM users WHERE id = 1`:                        "SEARCH users USING ROWID",
		`SELECT * FROM users WHERE id IN (1, 2)`:                  "SEARCH users USING ROWID (rowid IN",
		`SELECT * FROM users WHERE id > 1 AND id <= 3`:            "ROWID RANGE",
		`SELECT * FROM users WHERE email = 'a'`:                   "USING INDEX bangdb_autoindex_users_email",
		`SELECT * FROM orders WHERE user_id BETWEEN 1 AND 2`:      "INDEX idx_orders_user RANGE",
		`SELECT * FROM users WHERE name = 'kim'`:                  "SCAN users",
		`SELECT * FROM users u JOIN orders o ON o.user_id = u.id`: "SEARCH orders USING INDEX idx_orders_user",
		`DELETE FROM orders WHERE user_id = 1`:                    "SEARCH orders USING INDEX",
	}
	for q, want := range cases {
		if got := plan(q); !strings.Contains(got, want) {
			t.Errorf("%s: plan %q does not contain %q", q, got, want)
		}
	}
}

func TestUpdateDelete(t *testing.T) {
	db := setupShop(t)
	r := exec(t, db, `UPDATE users SET age = age + 1, email = UPPER(email) WHERE age IS NOT NULL`)
	if r.RowsAffected != 3 {
		t.Fatalf("rows affected = %d", r.RowsAffected)
	}
	expect(t, db, `SELECT name, age, email FROM users ORDER BY id`, "kim|32|KIM@X.IO;lee|26|LEE@X.IO;park|41|NULL;choi|NULL|choi@x.io")
	expect(t, db, `SELECT name FROM users WHERE email = 'KIM@X.IO'`, "kim") // index updated
	expect(t, db, `SELECT COUNT(*) FROM users WHERE email = 'kim@x.io'`, "0")

	// Changing the INTEGER PRIMARY KEY moves the row.
	exec(t, db, `UPDATE users SET id = 10 WHERE id = 4`)
	expect(t, db, `SELECT id, name FROM users WHERE id = 10`, "10|choi")
	expect(t, db, `SELECT name FROM users WHERE email = 'choi@x.io'`, "choi")

	r = exec(t, db, `DELETE FROM orders WHERE user_id = 1`)
	if r.RowsAffected != 2 {
		t.Fatalf("deleted %d", r.RowsAffected)
	}
	expect(t, db, `SELECT COUNT(*) FROM orders WHERE user_id = 1`, "0")
	expect(t, db, `SELECT COUNT(*) FROM orders`, "3")
	exec(t, db, `DELETE FROM orders`)
	expect(t, db, `SELECT COUNT(*) FROM orders`, "0")
}

func TestConstraints(t *testing.T) {
	db := setupShop(t)
	bad := []string{
		`INSERT INTO users (id, name) VALUES (1, 'dup-pk')`,
		`INSERT INTO users (name, email) VALUES ('x', 'kim@x.io')`,
		`INSERT INTO users (email) VALUES ('nobody@x.io')`,
		`UPDATE users SET email = 'lee@x.io' WHERE id = 1`,
		`UPDATE users SET id = 2 WHERE id = 1`,
		`UPDATE users SET name = NULL`,
	}
	for _, q := range bad {
		_, err := db.Exec(q)
		if !errors.Is(err, ErrConstraint) {
			t.Errorf("%s: want constraint error, got %v", q, err)
		}
	}
	typeErrs := []string{
		`INSERT INTO users (name, age) VALUES ('x', 'not a number')`,
		`INSERT INTO users (name, age) VALUES ('x', 1.5)`,
		`INSERT INTO nosuch VALUES (1)`,
		`INSERT INTO users (nosuch) VALUES (1)`,
		`INSERT INTO users (name) VALUES (1, 2)`,
	}
	for _, q := range typeErrs {
		if _, err := db.Exec(q); err == nil {
			t.Errorf("%s: expected error", q)
		}
	}
	// Failed statements must leave no trace (statement atomicity).
	expect(t, db, `SELECT COUNT(*) FROM users`, "4")
	_, err := db.Exec(`INSERT INTO users (name, email) VALUES ('a', 'a@x'), ('b', 'kim@x.io')`)
	if err == nil {
		t.Fatal("expected unique violation")
	}
	expect(t, db, `SELECT COUNT(*) FROM users WHERE email = 'a@x'`, "0")

	// Coercion: '42' into INT, 3 into REAL, number into TEXT.
	exec(t, db, `INSERT INTO users (name, age) VALUES (7, '42')`)
	expect(t, db, `SELECT name, TYPEOF(name), age, TYPEOF(age) FROM users WHERE name = '7'`, "7|text|42|integer")
	exec(t, db, `INSERT INTO orders (user_id, amount) VALUES (1, 3)`)
	expect(t, db, `SELECT TYPEOF(amount) FROM orders WHERE amount = 3`, "real")
}

func TestRowidAndAutoincrement(t *testing.T) {
	db := open(t)
	exec(t, db, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT); CREATE TABLE n (v TEXT)`)
	r := exec(t, db, `INSERT INTO t (v) VALUES ('a')`)
	if r.LastInsertID != 1 {
		t.Fatalf("first id = %d", r.LastInsertID)
	}
	exec(t, db, `INSERT INTO t VALUES (100, 'b')`)
	r = exec(t, db, `INSERT INTO t (v) VALUES ('c')`)
	if r.LastInsertID != 101 {
		t.Fatalf("id after explicit 100 = %d", r.LastInsertID)
	}
	// AUTOINCREMENT never reuses the id of a deleted maximum row, also
	// across transactions and after reopening.
	exec(t, db, `CREATE TABLE a (id INTEGER PRIMARY KEY AUTOINCREMENT, v TEXT)`)
	exec(t, db, `INSERT INTO a (v) VALUES ('1'), ('2'), ('3')`)
	exec(t, db, `DELETE FROM a WHERE id = 3`)
	if r := exec(t, db, `INSERT INTO a (v) VALUES ('4')`); r.LastInsertID != 4 {
		t.Fatalf("AUTOINCREMENT reused id: %d", r.LastInsertID)
	}
	exec(t, db, `DELETE FROM a`)
	if r := exec(t, db, `INSERT INTO a (v) VALUES ('5')`); r.LastInsertID != 5 {
		t.Fatalf("AUTOINCREMENT after delete-all: %d", r.LastInsertID)
	}
	// Plain INTEGER PRIMARY KEY may reuse (documented, like SQLite).
	exec(t, db, `DELETE FROM t WHERE id = 101`)
	if r := exec(t, db, `INSERT INTO t (v) VALUES ('x')`); r.LastInsertID != 101 {
		t.Fatalf("rowid table: %d", r.LastInsertID)
	}
	exec(t, db, `DROP TABLE a; CREATE TABLE a (id INTEGER PRIMARY KEY AUTOINCREMENT, v TEXT)`)
	if r := exec(t, db, `INSERT INTO a (v) VALUES ('fresh')`); r.LastInsertID != 1 {
		t.Fatalf("dropped table kept its sequence: %d", r.LastInsertID)
	}
	exec(t, db, `INSERT INTO n VALUES ('x'), ('y')`)
	expect(t, db, `SELECT rowid, v FROM n ORDER BY rowid DESC`, "2|y;1|x")
	expect(t, db, `SELECT * FROM n`, "x;y")
}

func TestDDL(t *testing.T) {
	db := setupShop(t)
	exec(t, db, `ALTER TABLE users ADD COLUMN active BOOLEAN DEFAULT TRUE`)
	expect(t, db, `SELECT name, active FROM users WHERE id = 1`, "kim|1")
	exec(t, db, `INSERT INTO users (name, active) VALUES ('new', FALSE)`)
	expect(t, db, `SELECT COUNT(*) FROM users WHERE active = 1`, "4")

	exec(t, db, `CREATE UNIQUE INDEX idx_name ON users (name)`)
	if _, err := db.Exec(`INSERT INTO users (name) VALUES ('kim')`); !errors.Is(err, ErrConstraint) {
		t.Fatalf("unique index not enforced: %v", err)
	}
	if _, err := db.Exec(`CREATE UNIQUE INDEX idx_item ON orders (item)`); err == nil {
		t.Fatal("unique index over duplicate data must fail")
	}
	exec(t, db, `DROP INDEX idx_name`)
	exec(t, db, `INSERT INTO users (name) VALUES ('kim')`)
	if _, err := db.Exec(`DROP INDEX bangdb_autoindex_users_email`); err == nil {
		t.Fatal("auto index must not be droppable")
	}
	exec(t, db, `DROP TABLE orders`)
	if _, err := db.Query(`SELECT * FROM orders`); err == nil {
		t.Fatal("dropped table still queryable")
	}
	exec(t, db, `DROP TABLE IF EXISTS orders; CREATE TABLE IF NOT EXISTS users (x INT)`)
	if _, err := db.Exec(`CREATE TABLE users (x INT)`); err == nil {
		t.Fatal("duplicate table must fail")
	}
	exec(t, db, `CREATE TABLE codes (code TEXT PRIMARY KEY, label TEXT)`)
	exec(t, db, `INSERT INTO codes VALUES ('KR', 'Korea'), ('JP', 'Japan')`)
	if _, err := db.Exec(`INSERT INTO codes VALUES ('KR', 'dup')`); !errors.Is(err, ErrConstraint) {
		t.Fatalf("text primary key not unique: %v", err)
	}
	expect(t, db, `SELECT label FROM codes WHERE code = 'JP'`, "Japan")
}

func TestTransactions(t *testing.T) {
	db := setupShop(t)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	tx.Exec(`INSERT INTO users (name) VALUES ('tx-user')`)
	tx.Exec(`CREATE TABLE tmp (a INT)`)
	// Uncommitted data is visible inside but not outside the transaction.
	if row := tx.QueryRow(`SELECT COUNT(*) FROM users`); true {
		var n int
		row.Scan(&n)
		if n != 5 {
			t.Fatalf("tx sees %d users", n)
		}
	}
	expect(t, db, `SELECT COUNT(*) FROM users`, "4")
	// A failing statement inside the tx is undone, the tx continues.
	if _, err := tx.Exec(`INSERT INTO users (id, name) VALUES (1, 'dup')`); err == nil {
		t.Fatal("expected error")
	}
	tx.Rollback()
	expect(t, db, `SELECT COUNT(*) FROM users`, "4")
	if _, err := db.Query(`SELECT * FROM tmp`); err == nil {
		t.Fatal("rolled back table exists")
	}

	err = db.Update(func(tx *Tx) error {
		_, err := tx.Exec(`UPDATE users SET age = 0`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	expect(t, db, `SELECT SUM(age) FROM users`, "0")

	// SQL-level BEGIN/COMMIT in a script.
	exec(t, db, `BEGIN; INSERT INTO users (name) VALUES ('s1'); INSERT INTO users (name) VALUES ('s2'); COMMIT;`)
	expect(t, db, `SELECT COUNT(*) FROM users`, "6")
	if _, err := db.Exec(`BEGIN; INSERT INTO users (name) VALUES ('s3')`); err == nil {
		t.Fatal("open transaction at end of script must be reported")
	}
	expect(t, db, `SELECT COUNT(*) FROM users`, "6")
	if err := tx.Commit(); !errors.Is(err, ErrTxDone) {
		t.Fatalf("commit after rollback: %v", err)
	}
}

func TestPersistenceAndLargeValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	exec(t, db, `CREATE TABLE docs (id INTEGER PRIMARY KEY, title TEXT, body TEXT, data BLOB)`)
	exec(t, db, `CREATE INDEX idx_title ON docs (title)`)
	big := strings.Repeat("가나다라마바사", 5000) // ~100 KB, spans overflow pages
	err = db.Update(func(tx *Tx) error {
		for i := 0; i < 2000; i++ {
			if _, err := tx.Exec(`INSERT INTO docs (title, body, data) VALUES (?, ?, ?)`,
				fmt.Sprintf("title-%04d", i), fmt.Sprintf("body %d", i), []byte{byte(i), 0, 1}); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`INSERT INTO docs (title, body) VALUES ('big', ?)`, big)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	expect(t, db, `SELECT COUNT(*) FROM docs`, "2001")
	expect(t, db, `SELECT id, body FROM docs WHERE title = 'title-1234'`, "1235|body 1234")
	var body string
	if err := db.QueryRow(`SELECT body FROM docs WHERE title = 'big'`).Scan(&body); err != nil || body != big {
		t.Fatalf("big value corrupted: %v (len %d)", err, len(body))
	}
	var data []byte
	db.QueryRow(`SELECT data FROM docs WHERE id = 3`).Scan(&data)
	if string(data) != "\x02\x00\x01" {
		t.Fatalf("blob = %v", data)
	}
	expect(t, db, `SELECT COUNT(*) FROM docs WHERE title >= 'title-1990' AND title < 'title-2000'`, "10")
	checkIntegrity(t, db)

	// Dropping and recreating reuses freed pages instead of growing the file.
	pagesBefore := db.Engine().PageCount()
	exec(t, db, `DROP TABLE docs`)
	exec(t, db, `CREATE TABLE docs2 (id INTEGER PRIMARY KEY, body TEXT)`)
	exec(t, db, `INSERT INTO docs2 (body) VALUES (?)`, big)
	if after := db.Engine().PageCount(); after > pagesBefore {
		t.Fatalf("file grew from %d to %d pages after drop", pagesBefore, after)
	}
}

func TestConcurrentReadersAndWriter(t *testing.T) {
	db := open(t)
	exec(t, db, `CREATE TABLE c (id INTEGER PRIMARY KEY, n INT)`)
	exec(t, db, `INSERT INTO c (n) VALUES (0)`)

	// A long write transaction does not block readers; they see the
	// committed snapshot.
	tx, _ := db.Begin()
	tx.Exec(`UPDATE c SET n = 1`)
	done := make(chan string)
	go func() { done <- query(t, db, `SELECT n FROM c`) }()
	select {
	case got := <-done:
		if got != "0" {
			t.Fatalf("reader saw uncommitted value %s", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reader blocked by writer")
	}
	tx.Commit()
	expect(t, db, `SELECT n FROM c`, "1")

	// Many writers and readers at once.
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if _, err := db.Exec(`UPDATE c SET n = n + 1 WHERE id = 1`); err != nil {
					errs <- err
					return
				}
				if _, err := db.Query(`SELECT n FROM c`); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	expect(t, db, `SELECT n FROM c`, "401")
}

func TestBusyTimeout(t *testing.T) {
	db, _ := OpenWithOptions(":memory:", &Options{BusyTimeout: 50 * time.Millisecond})
	defer db.Close()
	exec(t, db, `CREATE TABLE t (a INT)`)
	tx, _ := db.Begin()
	if _, err := db.Exec(`INSERT INTO t VALUES (1)`); !errors.Is(err, ErrBusy) {
		t.Fatalf("expected ErrBusy, got %v", err)
	}
	tx.Rollback()
	exec(t, db, `INSERT INTO t VALUES (1)`)
}

func TestScan(t *testing.T) {
	db := setupShop(t)
	var (
		id    int
		name  string
		email *string
		any_  any
		age   float64
	)
	_ = email
	if err := db.QueryRow(`SELECT id, name, email, age FROM users WHERE id = ?`, 3).Scan(&id, &name, &any_, &age); err != nil {
		t.Fatal(err)
	}
	if id != 3 || name != "park" || any_ != nil || age != 40 {
		t.Fatalf("scan: %v %v %v %v", id, name, any_, age)
	}
	if err := db.QueryRow(`SELECT id FROM users WHERE id = 999`).Scan(&id); !errors.Is(err, ErrNoRows) {
		t.Fatalf("want ErrNoRows, got %v", err)
	}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	exec(t, db, `CREATE TABLE ev (at TIMESTAMP, ok BOOLEAN)`)
	exec(t, db, `INSERT INTO ev VALUES (?, ?)`, now, true)
	var at time.Time
	var ok bool
	if err := db.QueryRow(`SELECT at, ok FROM ev`).Scan(&at, &ok); err != nil || !at.Equal(now) || !ok {
		t.Fatalf("time/bool round trip: %v %v %v", at, ok, err)
	}
}

// TestIndexMatchesScan compares indexed lookups with full scans on random
// data and random predicates.
func TestIndexMatchesScan(t *testing.T) {
	db := open(t)
	exec(t, db, `CREATE TABLE a (id INTEGER PRIMARY KEY, i INT, r REAL, s TEXT)`)
	exec(t, db, `CREATE TABLE b (id INTEGER PRIMARY KEY, i INT, r REAL, s TEXT)`)
	exec(t, db, `CREATE INDEX ai ON a (i); CREATE INDEX ar ON a (r); CREATE INDEX as_ ON a (s)`)
	rng := rand.New(rand.NewSource(7))
	randVal := func(col string) any {
		if rng.Intn(10) == 0 {
			return nil
		}
		switch col {
		case "i":
			return int64(rng.Intn(50) - 25)
		case "r":
			return float64(rng.Intn(40)-20) / 4
		}
		return fmt.Sprintf("s%02d", rng.Intn(30))
	}
	err := db.Update(func(tx *Tx) error {
		for k := 0; k < 600; k++ {
			i, r, s := randVal("i"), randVal("r"), randVal("s")
			for _, tbl := range []string{"a", "b"} {
				if _, err := tx.Exec(`INSERT INTO `+tbl+` (i, r, s) VALUES (?, ?, ?)`, i, r, s); err != nil {
					return err
				}
			}
		}
		// Random deletes and updates keep indexes busy.
		for k := 0; k < 100; k++ {
			id := rng.Intn(600) + 1
			v := randVal("i")
			for _, tbl := range []string{"a", "b"} {
				if _, err := tx.Exec(`UPDATE `+tbl+` SET i = ? WHERE id = ?`, v, id); err != nil {
					return err
				}
				if _, err := tx.Exec(`DELETE FROM `+tbl+` WHERE id = ?`, id+1); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ops := []string{"=", "<", "<=", ">", ">="}
	for k := 0; k < 400; k++ {
		col := []string{"i", "r", "s", "id"}[rng.Intn(4)]
		var cond string
		var args []any
		switch rng.Intn(3) {
		case 0:
			cond = fmt.Sprintf("%s %s ?", col, ops[rng.Intn(len(ops))])
			args = []any{randArg(rng, col, randVal)}
		case 1:
			cond = fmt.Sprintf("%s BETWEEN ? AND ?", col)
			args = []any{randArg(rng, col, randVal), randArg(rng, col, randVal)}
		default:
			cond = fmt.Sprintf("%s IN (?, ?, ?)", col)
			args = []any{randArg(rng, col, randVal), randArg(rng, col, randVal), randArg(rng, col, randVal)}
		}
		qa := `SELECT id, i, r, s FROM a WHERE ` + cond + ` ORDER BY id`
		qb := `SELECT id, i, r, s FROM b WHERE ` + cond + ` ORDER BY id`
		if ga, gb := query(t, db, qa, args...), query(t, db, qb, args...); ga != gb {
			t.Fatalf("index/scan mismatch for %s %v\nindex: %s\nscan:  %s", cond, args, ga, gb)
		}
	}
}

func randArg(rng *rand.Rand, col string, randVal func(string) any) any {
	switch rng.Intn(6) {
	case 0:
		return 2.5 // fractional number against INT columns
	case 1:
		return int64(rng.Intn(10))
	case 2:
		if col == "id" {
			return int64(rng.Intn(700))
		}
	}
	if col == "id" {
		return int64(rng.Intn(700))
	}
	return randVal(col)
}

func TestSelectWithoutFrom(t *testing.T) {
	db := open(t)
	expect(t, db, `SELECT 1 WHERE 0`, "")
	expect(t, db, `SELECT 'yes' WHERE 1 = 1`, "yes")
	expect(t, db, `SELECT COUNT(*) WHERE 0`, "0")
}
