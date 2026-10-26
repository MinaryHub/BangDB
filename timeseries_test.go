package bangdb

import (
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
)

// TestTimeSeriesMatchesTable runs the same queries on a time-series table
// and on an ordinary table holding the same rows.
func TestTimeSeriesMatchesTable(t *testing.T) {
	db := open(t)
	exec(t, db, `
		CREATE TABLE m (ts INTEGER NOT NULL, host TEXT, v REAL, n INT) USING timeseries WITH (time_column = ts, block_rows = 64);
		CREATE TABLE r (ts INTEGER NOT NULL, host TEXT, v REAL, n INT) USING btree;
		CREATE TABLE hosts (name TEXT PRIMARY KEY, dc TEXT);
		INSERT INTO hosts VALUES ('web-0', 'a'), ('web-1', 'a'), ('web-2', 'b')`)
	rng := rand.New(rand.NewSource(1))
	ts := int64(1_700_000_000)
	err := db.Update(func(tx *Tx) error {
		for i := 0; i < 5000; i++ {
			t := ts
			if rng.Intn(20) == 0 {
				t -= int64(rng.Intn(3000)) // late arrival
			} else {
				ts += int64(rng.Intn(3))
			}
			host := fmt.Sprintf("web-%d", rng.Intn(3))
			var v any = float64(rng.Intn(1000)) / 10
			if rng.Intn(30) == 0 {
				v = nil
			}
			for _, tb := range []string{"m", "r"} {
				if _, err := tx.Exec(`INSERT INTO `+tb+` VALUES (?, ?, ?, ?)`, t, host, v, i); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	same := func(q string, args ...any) {
		t.Helper()
		got := query(t, db, strings.ReplaceAll(q, "{t}", "m"), args...)
		want := query(t, db, strings.ReplaceAll(q, "{t}", "r"), args...)
		if got != want {
			t.Fatalf("%s %v: results differ\n time-series: %.200s\n table:       %.200s", q, args, got, want)
		}
	}
	for i := 0; i < 40; i++ {
		a := ts - 6000 + int64(rng.Intn(6000))
		b := a + int64(rng.Intn(800))
		same(`SELECT ts, host, v, n FROM {t} WHERE ts BETWEEN ? AND ? ORDER BY ts, n`, a, b)
		same(`SELECT n FROM {t} WHERE ts > ? AND ts <= ? AND host = 'web-1' ORDER BY n`, a, b)
		same(`SELECT ts, n FROM {t} WHERE ts >= ? ORDER BY ts DESC, n DESC LIMIT 7`, a)
		same(`SELECT count(*), round(sum(v), 6), min(ts), max(ts) FROM {t} WHERE ts < ?`, b) // rounded: rows are summed in another order
		same(`SELECT time_bucket(300, ts) AS b, host, count(*), round(avg(v), 3) FROM {t} WHERE ts >= ? GROUP BY b, host ORDER BY b, host`, a)
		same(`SELECT h.dc, count(*) FROM {t} JOIN hosts h ON h.name = {t}.host WHERE {t}.ts BETWEEN ? AND ? GROUP BY h.dc ORDER BY 1`, a, b)
		same(`SELECT n FROM {t} WHERE ts = ? ORDER BY n`, a)
		same(`SELECT n FROM {t} WHERE ts > 1.5e9 AND ts < ? + 0.5 ORDER BY n LIMIT 5`, a)
	}
	same(`SELECT ts FROM {t} ORDER BY ts LIMIT 3`)
	same(`SELECT ts FROM {t} ORDER BY ts DESC LIMIT 3`)
	same(`SELECT count(*) FROM {t} WHERE ts > 'text'`) // not a number: falls back to filtering

	// Plans: time conditions locate the rows; ORDER BY time needs no sort.
	if plan := query(t, db, `EXPLAIN SELECT * FROM m WHERE ts >= ? AND ts < ? ORDER BY ts DESC`, 1, 2); !strings.Contains(plan, "TIME RANGE") || strings.Contains(plan, "SORT") {
		t.Fatalf("plan: %s", plan)
	}

	// Changes: values, times (the row moves), deletions.
	for _, tb := range []string{"m", "r"} {
		exec(t, db, `UPDATE `+tb+` SET v = -1 WHERE n % 7 = 0`)
		exec(t, db, `UPDATE `+tb+` SET ts = ts + 100000 WHERE n % 11 = 0`)
		exec(t, db, `DELETE FROM `+tb+` WHERE n % 13 = 0`)
	}
	same(`SELECT ts, v, n FROM {t} ORDER BY ts, n`)
	resM := exec(t, db, `DELETE FROM m WHERE ts >= ? AND ts < ?`, ts-4000, ts-2000) // whole blocks
	resR := exec(t, db, `DELETE FROM r WHERE ts >= ? AND ts < ?`, ts-4000, ts-2000)
	if resM.RowsAffected != resR.RowsAffected || resM.RowsAffected == 0 {
		t.Fatalf("range delete: %d rows, want %d", resM.RowsAffected, resR.RowsAffected)
	}
	same(`SELECT ts, v, n FROM {t} ORDER BY ts, n`)
	same(`SELECT count(*) FROM {t}`)
}

func TestTimeSeriesRules(t *testing.T) {
	db := open(t)
	mustFail(t, db, `CREATE TABLE a (ts INTEGER, v REAL) USING timeseries`, "time_column")
	mustFail(t, db, `CREATE TABLE a (ts TEXT, v REAL) USING timeseries WITH (time_column = ts)`, "must be INTEGER")
	mustFail(t, db, `CREATE TABLE a (ts INTEGER, v REAL) USING timeseries WITH (time_column = ts, color = 'red')`, "unknown time-series option")
	mustFail(t, db, `CREATE TABLE a (ts INTEGER, v REAL) USING timeseries WITH (time_column = ts, time_unit = 'ns')`, "time_unit")
	mustFail(t, db, `CREATE TABLE a (ts INTEGER, v REAL UNIQUE) USING timeseries WITH (time_column = ts)`, "do not support indexes")
	mustFail(t, db, `CREATE TABLE a (id INTEGER PRIMARY KEY, ts INTEGER) USING timeseries WITH (time_column = ts)`, "do not support indexes")
	mustFail(t, db, `CREATE TABLE a (ts INTEGER) USING memory WITH (x = 1)`, "takes no options")
	exec(t, db, `CREATE TABLE a (ts INTEGER NOT NULL, v REAL) USING timeseries WITH (time_column = ts, time_unit = ms, retention = '1 hour')`)
	mustFail(t, db, `CREATE INDEX a_v ON a (v)`, "do not support indexes")
	mustFail(t, db, `INSERT INTO a VALUES (NULL, 1)`, "NOT NULL")
	mustFail(t, db, `ALTER TABLE a DROP COLUMN ts`, "time_column")
	exec(t, db, `ALTER TABLE a RENAME COLUMN ts TO at; ALTER TABLE a ADD COLUMN note TEXT DEFAULT 'x'`)
	exec(t, db, `INSERT INTO a (at, v) VALUES (1000, 1.5), (2000, 2.5)`)
	expect(t, db, `SELECT at, v, note FROM a WHERE at > 1500`, "2000|2.5|x")
	d := db.Engine().Catalog().Describe("a")
	if !strings.Contains(d, "USING timeseries WITH (retention = '1 hour', time_column = at, time_unit = ms)") {
		t.Fatalf("describe: %s", d)
	}
	// time_bucket
	expect(t, db, `SELECT time_bucket(300, 1000), time_bucket(300, -1), time_bucket('5 minutes', 1000), time_bucket('1 minute', 125000, 'ms')`, "900|-300|900|120000")
	expect(t, db, `SELECT time_bucket('15 minutes', '2026-10-25 13:52:10'), time_bucket('1 day', '2026-10-25T13:52:10Z')`, "2026-10-25 13:45:00|2026-10-25 00:00:00")
	mustFail(t, db, `SELECT time_bucket(0, 5)`, "positive")
}

func TestTimeSeriesRetentionAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ts.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	exec(t, db, `CREATE TABLE cpu (ts INTEGER NOT NULL, host TEXT, usage REAL) USING timeseries WITH (time_column = ts, retention = '1 day', block_rows = 256)`)
	err = db.Update(func(tx *Tx) error {
		for ts := int64(0); ts < 3*86400; ts += 60 {
			for h := 0; h < 4; h++ {
				if _, err := tx.Exec(`INSERT INTO cpu VALUES (?, ?, ?)`, ts, fmt.Sprintf("h%d", h), float64(ts%97)); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var lo, hi, n int64
	if err := db.QueryRow(`SELECT min(ts), max(ts), count(*) FROM cpu`).Scan(&lo, &hi, &n); err != nil {
		t.Fatal(err)
	}
	if hi-lo > 86400+3600 || hi-lo < 86400-3600 {
		t.Fatalf("retention kept %d..%d (%d rows)", lo, hi, n)
	}
	// Rollback undoes a transaction that sealed blocks.
	tx, _ := db.Begin()
	for i := 0; i < 1000; i++ {
		tx.Exec(`INSERT INTO cpu VALUES (?, 'x', 0)`, hi+int64(i))
	}
	tx.Rollback()
	expect(t, db, `SELECT count(*) FROM cpu`, fmt.Sprint(n))

	// A reader keeps its snapshot while new rows are sealed.
	rows, err := db.Query(`SELECT ts FROM cpu ORDER BY ts`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2000; i++ {
		exec(t, db, `INSERT INTO cpu VALUES (?, 'y', 1)`, hi+int64(i))
	}
	seen := int64(0)
	for rows.Next() {
		seen++
	}
	rows.Close()
	if seen != n {
		t.Fatalf("snapshot saw %d rows, want %d", seen, n)
	}
	// The 2000 newer rows moved the retention window forward.
	total := query(t, db, `SELECT count(*) FROM cpu`)
	if total == fmt.Sprint(n+2000) {
		t.Fatal("retention did not drop rows that fell out of the window")
	}
	checkIntegrity(t, db)
	if err := db.Vacuum(context.Background()); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if db, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	expect(t, db, `SELECT count(*) FROM cpu WHERE host = 'y'`, "2000")
	expect(t, db, `SELECT count(*) FROM cpu`, total)
	checkIntegrity(t, db)
}
