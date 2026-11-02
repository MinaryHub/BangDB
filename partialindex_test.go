package bangdb

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestPartialIndex(t *testing.T) {
	db := open(t)
	exec(t, db, `
		CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT, deleted_at INT, status TEXT);
		INSERT INTO users (email, status) VALUES ('a@x', 'active'), ('b@x', 'active');
		CREATE UNIQUE INDEX users_email_live ON users (email) WHERE deleted_at IS NULL;
		CREATE INDEX users_pending ON users (id) WHERE status = 'pending';`)

	// Uniqueness holds among live rows only.
	if _, err := db.Exec("INSERT INTO users (email) VALUES ('a@x')"); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("duplicate live email: %v", err)
	}
	exec(t, db, "UPDATE users SET deleted_at = 1 WHERE email = 'a@x'")
	exec(t, db, "INSERT INTO users (email, status) VALUES ('a@x', 'pending')") // the old one is deleted
	exec(t, db, "INSERT INTO users (email, deleted_at) VALUES ('a@x', 2)")     // so is this one
	if _, err := db.Exec("UPDATE users SET deleted_at = NULL WHERE id = 1"); err == nil {
		t.Fatal("reviving a duplicate email succeeded")
	}
	// A row entering the index through an UPDATE that leaves the key alone.
	exec(t, db, "UPDATE users SET status = 'pending' WHERE id = 2")
	expect(t, db, "SELECT id FROM users WHERE status = 'pending' ORDER BY id", "2;3")

	// The planner uses a partial index only when the query implies its WHERE.
	plan := query(t, db, "EXPLAIN SELECT id FROM users WHERE email = 'a@x' AND deleted_at IS NULL")
	if !strings.Contains(plan, "users_email_live") {
		t.Fatalf("partial index not used:\n%s", plan)
	}
	plan = query(t, db, "EXPLAIN SELECT id FROM users u WHERE u.email = 'a@x' AND u.deleted_at IS NULL")
	if !strings.Contains(plan, "users_email_live") {
		t.Fatalf("partial index not used with an alias:\n%s", plan)
	}
	plan = query(t, db, "EXPLAIN SELECT id FROM users WHERE email = 'a@x'")
	if strings.Contains(plan, "users_email_live") {
		t.Fatalf("partial index used without its condition:\n%s", plan)
	}
	expect(t, db, "SELECT id FROM users WHERE email = 'a@x' ORDER BY id", "1;3;4")
	expect(t, db, "SELECT id FROM users WHERE email = 'a@x' AND deleted_at IS NULL", "3")
	expect(t, db, "SELECT id FROM users ORDER BY id LIMIT 2", "1;2") // not via the partial index on id

	exec(t, db, "DELETE FROM users WHERE id = 3")
	exec(t, db, "INSERT INTO users (email) VALUES ('a@x')")
	expect(t, db, "PRAGMA integrity_check", "ok")

	// Columns used by the condition cannot be dropped; renames follow.
	if _, err := db.Exec("ALTER TABLE users DROP COLUMN deleted_at"); err == nil || !strings.Contains(err.Error(), "users_email_live") {
		t.Fatalf("drop column used by an index WHERE: %v", err)
	}
	exec(t, db, "ALTER TABLE users RENAME COLUMN deleted_at TO removed_at")
	if _, err := db.Exec("INSERT INTO users (email) VALUES ('a@x')"); err == nil {
		t.Fatal("unique partial index lost after rename")
	}

	// Dump and reload keep the condition.
	var buf bytes.Buffer
	if err := db.Dump(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `ON users (email) WHERE removed_at IS NULL;`) {
		t.Fatalf("dump lacks the index condition:\n%s", buf.String())
	}
	db2 := open(t)
	if _, err := db2.ImportSQL(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	expect(t, db2, "PRAGMA integrity_check", "ok")
	if _, err := db2.Exec("INSERT INTO users (email) VALUES ('a@x')"); err == nil {
		t.Fatal("reloaded partial index does not enforce uniqueness")
	}

	// Conditions with subqueries or parameters are refused.
	if _, err := db.Exec("CREATE INDEX bad ON users (email) WHERE id IN (SELECT 1)"); err == nil {
		t.Fatal("subquery in index WHERE accepted")
	}
	// A partial UNIQUE index is not a key for foreign keys.
	if _, err := db.Exec("CREATE TABLE ref (e TEXT REFERENCES users (email))"); err == nil {
		t.Fatal("foreign key to a partial unique index accepted")
	}
}

func TestPartialIndexReopen(t *testing.T) {
	durableOnly(t)
	path := t.TempDir() + "/p.db"
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	exec(t, db, `CREATE TABLE q (id INTEGER PRIMARY KEY, job TEXT, done INT DEFAULT 0);
		CREATE INDEX q_todo ON q (job) WHERE done = 0;
		INSERT INTO q (job) VALUES ('a'), ('b');
		UPDATE q SET done = 1 WHERE job = 'a';`)
	db.Close()
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec(t, db, "ALTER TABLE q ADD COLUMN prio INT DEFAULT 5")
	exec(t, db, "CREATE INDEX q_prio ON q (job) WHERE prio = 5") // old rows get the default
	expect(t, db, "SELECT job FROM q WHERE prio = 5 AND job >= '' ORDER BY job", "a;b")
	expect(t, db, "SELECT job FROM q WHERE done = 0 AND job >= ''", "b")
	expect(t, db, "PRAGMA integrity_check", "ok")
	if plan := query(t, db, "EXPLAIN SELECT job FROM q WHERE done = 0 AND job = 'b'"); !strings.Contains(plan, "q_todo") {
		t.Fatalf("partial index not used after reopen:\n%s", plan)
	}
}
