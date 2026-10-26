// Command tsbench is the time-series benchmark of ROADMAP phase 5: it
// ingests 10 million points (1,000 hosts every 10 s) into a time-series
// table, 2 million into an ordinary table for comparison, and times typical
// queries. Run: go run ./internal/tsbench
package main

import (
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/knoxxr/bangdb"
)

func size(path string) float64 {
	a, _ := os.Stat(path)
	return float64(a.Size()) / 1e6
}

func ingest(db *bangdb.DB, table string, hosts, steps int) time.Duration {
	rng := rand.New(rand.NewSource(1))
	usage := make([]float64, hosts)
	start := time.Now()
	const batch = 500
	t0 := int64(1_760_000_000)
	var sb strings.Builder
	args := make([]any, 0, batch*4)
	tx, _ := db.Begin()
	n := 0
	flush := func() {
		if len(args) == 0 {
			return
		}
		if _, err := tx.Exec(sb.String(), args...); err != nil {
			panic(err)
		}
		sb.Reset()
		args = args[:0]
	}
	for s := 0; s < steps; s++ {
		ts := t0 + int64(s)*10
		for h := 0; h < hosts; h++ {
			usage[h] += rng.NormFloat64()
			if sb.Len() == 0 {
				sb.WriteString("INSERT INTO " + table + " VALUES ")
			} else {
				sb.WriteString(",")
			}
			sb.WriteString("(?,?,?,?)")
			args = append(args, ts, fmt.Sprintf("host-%04d", h), usage[h], int64(s*hosts+h))
			if len(args) == batch*4 {
				flush()
			}
			if n++; n%200_000 == 0 {
				flush()
				if err := tx.Commit(); err != nil {
					panic(err)
				}
				tx, _ = db.Begin()
			}
		}
	}
	flush()
	if err := tx.Commit(); err != nil {
		panic(err)
	}
	return time.Since(start)
}

func timeQuery(db *bangdb.DB, q string, args ...any) (time.Duration, string) {
	start := time.Now()
	rows, err := db.Query(q, args...)
	if err != nil {
		panic(err)
	}
	n := 0
	var first []any
	for rows.Next() {
		if n == 0 {
			all := rows.Values()
			first = all
		}
		n++
	}
	if err := rows.Err(); err != nil {
		panic(err)
	}
	return time.Since(start), fmt.Sprintf("%d rows, first %v", n, first)
}

func main() {
	dir, _ := os.MkdirTemp("", "tsbench")
	defer os.RemoveAll(dir)
	for _, c := range []struct {
		kind         string
		hosts, steps int
	}{{"timeseries", 1000, 10000}, {"btree", 1000, 2000}} {
		path := dir + "/" + c.kind + ".db"
		db, err := bangdb.Open(path)
		if err != nil {
			panic(err)
		}
		ddl := `CREATE TABLE cpu (ts INTEGER NOT NULL, host TEXT, usage REAL, seq INT) USING timeseries WITH (time_column = ts)`
		if c.kind == "btree" {
			ddl = `CREATE TABLE cpu (ts INTEGER NOT NULL, host TEXT, usage REAL, seq INT); CREATE INDEX cpu_ts ON cpu (ts)`
		}
		if _, err := db.Exec(ddl); err != nil {
			panic(err)
		}
		points := c.hosts * c.steps
		d := ingest(db, "cpu", c.hosts, c.steps)
		db.Engine().Checkpoint()
		fmt.Printf("== %s: %d points ingested in %v (%.0f points/s), file %.1f MB (%.1f bytes/point)\n",
			c.kind, points, d.Round(time.Millisecond), float64(points)/d.Seconds(), size(path), size(path)*1e6/float64(points))
		end := int64(1_760_000_000) + int64(c.steps-1)*10
		for _, q := range []struct {
			name string
			sql  string
			args []any
		}{
			{"last 5 min, all hosts: avg", `SELECT avg(usage), count(*) FROM cpu WHERE ts > ?`, []any{end - 300}},
			{"last 1 h, one host, 5-min buckets", `SELECT time_bucket(300, ts) b, avg(usage), max(usage) FROM cpu WHERE ts > ? AND host = 'host-0042' GROUP BY b ORDER BY b`, []any{end - 3600}},
			{"last 1 h, all hosts, per host avg", `SELECT host, avg(usage) FROM cpu WHERE ts > ? GROUP BY host`, []any{end - 3600}},
			{"latest 10 points", `SELECT ts, host, usage FROM cpu ORDER BY ts DESC LIMIT 10`, nil},
			{"whole range, 1-hour buckets", `SELECT time_bucket(3600, ts) b, count(*), avg(usage) FROM cpu GROUP BY b ORDER BY b`, nil},
		} {
			d, info := timeQuery(db, q.sql, q.args...)
			fmt.Printf("   %-36s %10v  (%s)\n", q.name, d.Round(time.Microsecond), info)
		}
		start := time.Now()
		res, err := db.Exec(`DELETE FROM cpu WHERE ts < ?`, int64(1_760_000_000)+3600)
		if err != nil {
			panic(err)
		}
		fmt.Printf("   %-36s %10v  (%d rows)\n", "delete the oldest hour", time.Since(start).Round(time.Microsecond), res.RowsAffected)
		db.Close()
	}
}
