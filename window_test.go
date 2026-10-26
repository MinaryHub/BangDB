package bangdb

import (
	"strings"
	"testing"
)

func setupScores(t *testing.T) *DB {
	db := open(t)
	exec(t, db, "CREATE TABLE s (id INTEGER PRIMARY KEY, team TEXT, name TEXT, pts INTEGER)")
	exec(t, db, `INSERT INTO s VALUES
		(1, 'a', 'ann', 10), (2, 'a', 'bob', 30), (3, 'a', 'cy', 30), (4, 'a', 'dee', 5),
		(5, 'b', 'eve', 7), (6, 'b', 'fay', 9), (7, 'b', 'gus', NULL)`)
	return db
}

func TestWindowRanking(t *testing.T) {
	db := setupScores(t)
	expect(t, db, `SELECT name, row_number() OVER (ORDER BY pts DESC, name), rank() OVER (ORDER BY pts DESC),
			dense_rank() OVER (ORDER BY pts DESC) FROM s WHERE team = 'a' ORDER BY id`,
		"ann|3|3|2;bob|1|1|1;cy|2|1|1;dee|4|4|3")
	expect(t, db, `SELECT name, rank() OVER (PARTITION BY team ORDER BY pts) FROM s ORDER BY team, 2, name`,
		"dee|1;ann|2;bob|3;cy|3;gus|1;eve|2;fay|3")
	expect(t, db, `SELECT name, percent_rank() OVER (ORDER BY pts), cume_dist() OVER (ORDER BY pts)
			FROM s WHERE team = 'a' ORDER BY id`,
		"ann|0.3333333333333333|0.5;bob|0.6666666666666666|1;cy|0.6666666666666666|1;dee|0|0.25")
	expect(t, db, `SELECT id, ntile(3) OVER (ORDER BY id) FROM s`, "1|1;2|1;3|1;4|2;5|2;6|3;7|3")
	expect(t, db, `SELECT id, ntile(10) OVER (ORDER BY id) FROM s WHERE id <= 3`, "1|1;2|2;3|3")
	// Window values can drive ORDER BY and LIMIT, and windows see the rows
	// after WHERE.
	expect(t, db, `SELECT name FROM s ORDER BY row_number() OVER (ORDER BY name DESC) LIMIT 2`, "gus;fay")
}

func TestWindowOffsets(t *testing.T) {
	db := setupScores(t)
	expect(t, db, `SELECT id, lag(pts) OVER (ORDER BY id), lead(pts, 2, -1) OVER (ORDER BY id) FROM s WHERE team = 'a'`,
		"1|NULL|30;2|10|5;3|30|-1;4|30|-1")
	expect(t, db, `SELECT id, lag(name, 1, 'none') OVER (PARTITION BY team ORDER BY id) FROM s`,
		"1|none;2|ann;3|bob;4|cy;5|none;6|eve;7|fay")
}

func TestWindowAggregates(t *testing.T) {
	db := setupScores(t)
	// Running total (default frame: up to the current row's peers).
	expect(t, db, `SELECT id, sum(pts) OVER (ORDER BY pts, id) FROM s WHERE team = 'a' ORDER BY id`,
		"1|15;2|45;3|75;4|5")
	expect(t, db, `SELECT id, sum(pts) OVER (ORDER BY pts) FROM s WHERE team = 'a' ORDER BY id`,
		"1|15;2|75;3|75;4|5")
	// Whole partition without ORDER BY.
	expect(t, db, `SELECT id, count(*) OVER (PARTITION BY team), avg(pts) OVER (PARTITION BY team),
			count(pts) OVER (PARTITION BY team) FROM s ORDER BY id`,
		"1|4|18.75|4;2|4|18.75|4;3|4|18.75|4;4|4|18.75|4;5|3|8|2;6|3|8|2;7|3|8|2")
	// Sliding ROWS frames.
	expect(t, db, `SELECT id, sum(pts) OVER (ORDER BY id ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM s`,
		"1|40;2|70;3|65;4|42;5|21;6|16;7|9")
	expect(t, db, `SELECT id, max(pts) OVER (ORDER BY id ROWS 2 PRECEDING) FROM s WHERE team = 'a'`,
		"1|10;2|30;3|30;4|30")
	expect(t, db, `SELECT id, sum(pts) OVER (ORDER BY id ROWS BETWEEN CURRENT ROW AND UNBOUNDED FOLLOWING) FROM s
			WHERE team = 'b'`, "5|16;6|9;7|NULL")
	expect(t, db, `SELECT id, first_value(name) OVER (ORDER BY id ROWS BETWEEN 1 FOLLOWING AND 2 FOLLOWING),
			last_value(name) OVER (ORDER BY id ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING),
			nth_value(name, 2) OVER (ORDER BY id) FROM s WHERE team = 'b'`,
		"5|fay|gus|NULL;6|gus|gus|fay;7|NULL|gus|fay")
	expect(t, db, `SELECT id, group_concat(name) OVER (ORDER BY id ROWS BETWEEN 1 PRECEDING AND CURRENT ROW)
			FROM s WHERE team = 'b'`, "5|eve;6|eve,fay;7|fay,gus")
	// Frame offsets may be parameters.
	expect(t, db, `SELECT id, count(*) OVER (ORDER BY id ROWS ? PRECEDING) FROM s WHERE id < 4`, "1|1;2|2;3|3", 5)
}

func TestWindowOverGroups(t *testing.T) {
	db := setupScores(t)
	expect(t, db, `SELECT team, sum(pts), rank() OVER (ORDER BY sum(pts) DESC),
			sum(sum(pts)) OVER () FROM s GROUP BY team ORDER BY team`,
		"a|75|1|91;b|16|2|91")
	expect(t, db, `SELECT DISTINCT team, count(*) OVER (PARTITION BY team) FROM s ORDER BY 1`, "a|4;b|3")
	expect(t, db, `SELECT * FROM (SELECT name, row_number() OVER (PARTITION BY team ORDER BY pts DESC) AS r FROM s) t
			WHERE r = 1 ORDER BY name`, "bob|1;fay|1")
}

func TestWindowErrors(t *testing.T) {
	db := setupScores(t)
	for q, want := range map[string]string{
		"SELECT name FROM s WHERE row_number() OVER () > 1":                       "misuse of window function",
		"SELECT team FROM s GROUP BY team HAVING rank() OVER (ORDER BY team) = 1": "misuse of window function",
		"SELECT row_number() FROM s":                                              "requires an OVER clause",
		"SELECT abs(pts) OVER () FROM s":                                          "not a window function",
		"SELECT sum(pts) OVER (ORDER BY id RANGE 1 PRECEDING) FROM s":             "not supported",
		"SELECT sum(pts) OVER (ROWS BETWEEN CURRENT ROW AND 1 PRECEDING) FROM s":  "invalid window frame",
		"SELECT ntile(0) OVER () FROM s":                                          "positive integer",
		"SELECT sum(row_number() OVER ()) OVER () FROM s":                         "misuse of window function",
		"SELECT id, first_value(name) OVER w FROM s":                              "", // named windows are not supported
	} {
		rows, err := db.Query(q)
		if err == nil {
			for rows.Next() {
			}
			err = rows.Err()
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", q, err, want)
		}
	}
}
