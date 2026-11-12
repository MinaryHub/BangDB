package bangdb

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestJSONEach(t *testing.T) {
	db := open(t)
	// One row per member: arrays by position, objects by key.
	expect(t, db, `SELECT key, value, type, atom, id, parent, fullkey, path FROM json_each('[10, "x", null, true, 2.5, [1], {"a":1}]')`,
		`0|10|integer|10|1|NULL|$[0]|$;1|x|text|x|2|NULL|$[1]|$;2|NULL|null|NULL|3|NULL|$[2]|$;3|1|true|1|4|NULL|$[3]|$;`+
			`4|2.5|real|2.5|5|NULL|$[4]|$;5|[1]|array|NULL|6|NULL|$[5]|$;6|{"a":1}|object|NULL|7|NULL|$[6]|$`)
	expect(t, db, `SELECT key, value, fullkey FROM json_each('{"a":1,"b c":[2,3],"d":{"e":4}}')`,
		`a|1|$.a;b c|[2,3]|$."b c";d|{"e":4}|$.d`)
	// A path starts lower in the document; a scalar is one row; a missing
	// path, NULL and an empty container are none.
	expect(t, db, `SELECT key, value, fullkey, path FROM json_each('{"a":{"b":[7,8]}}', '$.a.b')`, "0|7|$.a.b[0]|$.a.b;1|8|$.a.b[1]|$.a.b")
	expect(t, db, `SELECT key, value, fullkey, path FROM json_each('{"a":{"b":[7,8]}}', '$.a.b[#-1]')`, "1|8|$.a.b[1]|$.a.b")
	expect(t, db, `SELECT key, value, type, fullkey, path FROM json_each('"solo"')`, "NULL|solo|text|$|$")
	expect(t, db, `SELECT COUNT(*) FROM json_each('{"a":1}', '$.zz')`, "0")
	expect(t, db, `SELECT COUNT(*) FROM json_each(NULL)`, "0")
	expect(t, db, `SELECT COUNT(*) FROM json_each('[]')`, "0")
	expect(t, db, `SELECT COUNT(*) FROM json_each('[1]', NULL)`, "0")
	// The default name and an alias; SELECT * gives the eight columns.
	expect(t, db, `SELECT json_each.value FROM json_each('[1,2]') WHERE json_each.key = 1`, "2")
	expect(t, db, `SELECT * FROM json_each('[5]') AS j`, "0|5|integer|5|1|NULL|$[0]|$")
	expect(t, db, `SELECT SUM(j.value), MAX(j.key) FROM json_each('[1,2,3,4]') j WHERE j.value > 1`, "9|3")
	expect(t, db, `SELECT value FROM json_each('[3,1,2]') ORDER BY value DESC LIMIT 2`, "3;2")
	expect(t, db, `SELECT value FROM json_each(?) ORDER BY 1`, "a;b", `["b","a"]`)

	// json_tree: the element and everything below it, parents first.
	expect(t, db, `SELECT id, parent, key, type, fullkey, path FROM json_tree('{"a":[1,{"b":2}],"c":"x"}')`,
		`0|NULL|NULL|object|$|$;1|0|a|array|$.a|$;2|1|0|integer|$.a[0]|$.a;3|1|1|object|$.a[1]|$.a;`+
			`4|3|b|integer|$.a[1].b|$.a[1];5|0|c|text|$.c|$`)
	expect(t, db, `SELECT fullkey, atom FROM json_tree('{"a":[1,{"b":2}],"c":"x"}') WHERE atom IS NOT NULL`, "$.a[0]|1;$.a[1].b|2;$.c|x")
	expect(t, db, `SELECT key, fullkey, path FROM json_tree('{"a":{"b":[7]}}', '$.a')`, "a|$.a|$;b|$.a.b|$.a;0|$.a.b[0]|$.a.b")
	expect(t, db, `SELECT fullkey FROM json_tree('[[1,2],[3]]') WHERE type = 'integer' LIMIT 2`, "$[0][0];$[0][1]")

	// Lateral: the argument comes from the table to the left.
	exec(t, db, `
		CREATE TABLE posts (id INTEGER PRIMARY KEY, title TEXT, tags TEXT, meta TEXT);
		INSERT INTO posts VALUES
			(1, 'go', '["db","go"]', '{"views":10,"by":{"name":"kim"}}'),
			(2, 'sql', '["db","sql","go"]', '{"views":25}'),
			(3, 'none', '[]', NULL),
			(4, 'null', NULL, '{"views":1}');`)
	expect(t, db, `SELECT p.id, j.value FROM posts p, json_each(p.tags) j ORDER BY p.id, j.key`, "1|db;1|go;2|db;2|sql;2|go")
	expect(t, db, `SELECT p.title FROM posts p JOIN json_each(p.tags) j ON j.value = 'go' ORDER BY p.id`, "go;sql")
	expect(t, db, `SELECT j.value, COUNT(*) FROM posts p, json_each(p.tags) j GROUP BY j.value ORDER BY 2 DESC, 1`, "db|2;go|2;sql|1")
	expect(t, db, `SELECT p.id, j.value FROM posts p LEFT JOIN json_each(p.tags) j ON 1 ORDER BY p.id, j.key`,
		"1|db;1|go;2|db;2|sql;2|go;3|NULL;4|NULL")
	expect(t, db, `SELECT DISTINCT p.id FROM posts p, json_each(p.tags) a, json_each(p.tags) b WHERE a.value = 'db' AND b.value = 'sql'`, "2")
	expect(t, db, `SELECT p.id, t.fullkey, t.atom FROM posts p, json_tree(p.meta, '$.by') t WHERE t.atom IS NOT NULL`, "1|$.by.name|kim")
	expect(t, db, `SELECT id FROM posts p WHERE EXISTS (SELECT 1 FROM json_each(p.tags) WHERE value = 'sql')`, "2")
	expect(t, db, `SELECT id, (SELECT COUNT(*) FROM json_each(p.tags)) FROM posts p ORDER BY id`, "1|2;2|3;3|0;4|0")
	// Functions of functions, and with other features.
	expect(t, db, `SELECT o.key, i.value FROM json_each('{"x":[1,2],"y":[3]}') o, json_each(o.value) i`, "x|1;x|2;y|3")
	expect(t, db, `WITH tags AS (SELECT p.id, j.value AS tag FROM posts p, json_each(p.tags) j)
		SELECT tag, GROUP_CONCAT(id) FROM tags GROUP BY tag ORDER BY tag`, "db|1,2;go|1,2;sql|2")
	exec(t, db, `CREATE TABLE tagged (post INT, tag TEXT);
		INSERT INTO tagged SELECT p.id, j.value FROM posts p, json_each(p.tags) j`)
	expect(t, db, `SELECT COUNT(*) FROM tagged`, "5")
	exec(t, db, `CREATE VIEW post_tags AS SELECT p.id, j.value AS tag FROM posts p, json_each(p.tags) j`)
	expect(t, db, `SELECT id FROM post_tags WHERE tag = 'sql'`, "2")
	explainHas(t, db, `SELECT 1 FROM posts p, json_each(p.tags) j`, "SCAN FUNCTION json_each")

	for _, q := range []string{
		`SELECT * FROM json_each('{bad')`,
		`SELECT * FROM json_each()`,
		`SELECT * FROM json_each('[]', '$', 1)`,
		`SELECT * FROM json_each('[]', 'nope')`,
		`SELECT * FROM no_such_fn(1)`,
		`SELECT * FROM json_each(p.tags) j, posts p`, // the argument's table comes later
		`SELECT * FROM json_each((SELECT tags FROM posts LIMIT 1))`,
		`SELECT * FROM json_each(x'00')`,
	} {
		if _, err := db.Exec(q); err == nil { // Exec runs the query to its end
			t.Errorf("%s: expected error", q)
		}
	}
}

func TestGenerateSeries(t *testing.T) {
	db := open(t)
	expect(t, db, `SELECT value FROM generate_series(1, 5)`, "1;2;3;4;5")
	expect(t, db, `SELECT value FROM generate_series(10, 1, -3)`, "10;7;4;1")
	expect(t, db, `SELECT value FROM generate_series(0, 10, 5)`, "0;5;10")
	expect(t, db, `SELECT COUNT(*) FROM generate_series(5, 1)`, "0")
	expect(t, db, `SELECT COUNT(*) FROM generate_series(1, NULL)`, "0")
	expect(t, db, `SELECT COUNT(*), SUM(value) FROM generate_series(1, 100000)`, "100000|5000050000")
	expect(t, db, `SELECT value FROM generate_series(9223372036854775806, 9223372036854775807)`, "9223372036854775806;9223372036854775807")
	expect(t, db, `SELECT value FROM generate_series(1, 1000000000) LIMIT 3`, "1;2;3") // streamed, not built
	expect(t, db, `SELECT a.value, b.value FROM generate_series(1, 2) a, generate_series(a.value, 2) b`, "1|1;1|2;2|2")
	exec(t, db, `CREATE TABLE n (v INT); INSERT INTO n SELECT value FROM generate_series(1, 50)`)
	expect(t, db, `SELECT COUNT(*) FROM n JOIN generate_series(49, 60) g ON g.value = n.v`, "2")
	if _, err := db.Exec(`SELECT * FROM generate_series('a', 2)`); err == nil {
		t.Error("generate_series('a', 2): expected error")
	}
	// An endless series is stopped by the timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := db.ExecContext(ctx, `SELECT COUNT(*) FROM generate_series(1, 9223372036854775807)`)
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Errorf("endless series: %v", err)
	}
}
