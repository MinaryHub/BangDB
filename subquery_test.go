package bangdb

import "testing"

func TestSubqueries(t *testing.T) {
	db := setupShop(t) // users(id,name,email,age), orders(id,user_id,item,amount)

	// Scalar subqueries: uncorrelated and correlated.
	expect(t, db, `SELECT name FROM users WHERE age = (SELECT MAX(age) FROM users)`, "park")
	expect(t, db, `SELECT name, (SELECT COUNT(*) FROM orders o WHERE o.user_id = u.id) AS n FROM users u ORDER BY id`,
		"kim|2;lee|1;park|1;choi|0")
	expect(t, db, `SELECT (SELECT name FROM users WHERE id = 99)`, "NULL")
	// EXISTS / NOT EXISTS
	expect(t, db, `SELECT name FROM users u WHERE EXISTS (SELECT 1 FROM orders WHERE user_id = u.id AND amount > 100)`, "park")
	expect(t, db, `SELECT name FROM users u WHERE NOT EXISTS (SELECT 1 FROM orders o WHERE o.user_id = u.id)`, "choi")
	// IN / NOT IN, including NULL semantics
	expect(t, db, `SELECT name FROM users WHERE id IN (SELECT user_id FROM orders WHERE item = 'book') ORDER BY id`, "kim;lee")
	expect(t, db, `SELECT item FROM orders WHERE user_id NOT IN (SELECT id FROM users) ORDER BY id`, "ghost")
	exec(t, db, `CREATE TABLE maybe (v INT); INSERT INTO maybe VALUES (1), (NULL)`)
	expect(t, db, `SELECT COUNT(*) FROM users WHERE id NOT IN (SELECT v FROM maybe)`, "0") // NULL makes NOT IN unknown
	expect(t, db, `SELECT COUNT(*) FROM users WHERE id IN (SELECT v FROM maybe)`, "1")
	expect(t, db, `SELECT 2.0 IN (SELECT id FROM users)`, "1") // 2.0 = 2
	// Nested, correlated two levels up
	expect(t, db, `SELECT name FROM users u WHERE EXISTS (
		SELECT 1 FROM orders o WHERE o.user_id = u.id AND EXISTS (
			SELECT 1 FROM orders o2 WHERE o2.item = o.item AND o2.user_id <> u.id))
		ORDER BY name`, "kim;lee")
	// In joins, in HAVING, in ORDER BY
	expect(t, db, `SELECT u.name, o.item FROM users u JOIN orders o ON o.user_id = u.id
		WHERE o.amount = (SELECT MAX(amount) FROM orders o3 WHERE o3.user_id = u.id) ORDER BY u.id`,
		"kim|book;lee|book;park|laptop")
	expect(t, db, `SELECT user_id, SUM(amount) FROM orders GROUP BY user_id
		HAVING SUM(amount) > (SELECT AVG(amount) FROM orders) ORDER BY user_id`, "3|1200")
	// Derived tables
	expect(t, db, `SELECT s.user_id, s.total FROM (SELECT user_id, SUM(amount) AS total FROM orders GROUP BY user_id) AS s
		WHERE s.total > 10 ORDER BY s.total DESC`, "3|1200;1|14.5;2|12.5")
	expect(t, db, `SELECT u.name, t.total FROM users u JOIN (SELECT user_id, SUM(amount) total FROM orders GROUP BY user_id) t
		ON t.user_id = u.id ORDER BY u.id`, "kim|14.5;lee|12.5;park|1200")
	expect(t, db, `SELECT COUNT(*) FROM (SELECT * FROM users WHERE age > 100) x`, "0")
	// DML with subqueries
	exec(t, db, `UPDATE users SET age = (SELECT COUNT(*) FROM orders o WHERE o.user_id = users.id) WHERE id IN (SELECT user_id FROM orders)`)
	expect(t, db, `SELECT id, age FROM users ORDER BY id`, "1|2;2|1;3|1;4|NULL")
	exec(t, db, `DELETE FROM orders WHERE user_id NOT IN (SELECT id FROM users)`)
	expect(t, db, `SELECT COUNT(*) FROM orders`, "4")
	exec(t, db, `INSERT INTO orders (user_id, item, amount) VALUES ((SELECT id FROM users WHERE name = 'choi'), 'pen', 1)`)
	expect(t, db, `SELECT user_id FROM orders WHERE item = 'pen' ORDER BY id`, "1;4")

	// Errors
	for _, q := range []string{
		`SELECT (SELECT id, name FROM users)`,
		`SELECT * FROM users WHERE id IN (SELECT id, name FROM users)`,
		`SELECT * FROM (SELECT 1)`, // derived table needs an alias
	} {
		if _, err := db.Query(q); err == nil {
			t.Errorf("%s: expected error", q)
		}
	}
}

func TestSetOperations(t *testing.T) {
	db := open(t)
	exec(t, db, `CREATE TABLE a (v INT); CREATE TABLE b (v INT);
		INSERT INTO a VALUES (1), (2), (2), (3);
		INSERT INTO b VALUES (2), (3), (4)`)
	expect(t, db, `SELECT v FROM a UNION SELECT v FROM b ORDER BY v`, "1;2;3;4")
	expect(t, db, `SELECT v FROM a UNION ALL SELECT v FROM b ORDER BY 1 DESC LIMIT 3`, "4;3;3")
	expect(t, db, `SELECT v FROM a INTERSECT SELECT v FROM b ORDER BY v`, "2;3")
	expect(t, db, `SELECT v FROM a EXCEPT SELECT v FROM b`, "1")
	expect(t, db, `SELECT v AS x FROM a UNION SELECT v FROM b ORDER BY x DESC LIMIT 2 OFFSET 1`, "3;2")
	expect(t, db, `SELECT v FROM a UNION SELECT v * 10 FROM b EXCEPT SELECT 40 ORDER BY v`, "1;2;3;20;30")
	expect(t, db, `SELECT COUNT(*) FROM (SELECT v FROM a UNION SELECT v FROM b) u`, "4")
	expect(t, db, `SELECT v FROM a WHERE v IN (SELECT v FROM b UNION SELECT 1) ORDER BY v`, "1;2;2;3")
	if _, err := db.Query(`SELECT v FROM a UNION SELECT v, v FROM b`); err == nil {
		t.Error("column count mismatch must fail")
	}
	if _, err := db.Query(`SELECT v FROM a UNION SELECT v FROM b ORDER BY nope`); err == nil {
		t.Error("ORDER BY on unknown compound column must fail")
	}
}
