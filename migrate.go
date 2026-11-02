package bangdb

import (
	"context"
	"fmt"
	"sort"
)

// Migration is one step of a schema's history, applied once.
type Migration struct {
	// Version identifies the step; the database records the last applied
	// version in PRAGMA user_version. Versions must be positive and unique.
	Version int
	// Name describes the step (for errors).
	Name string
	// SQL is run in the step's transaction (several statements allowed)...
	SQL string
	// ...followed by Func, if set, for changes that need code.
	Func func(tx *Tx) error
}

// Migrate brings the schema up to date: it applies, in version order, every
// migration newer than the database's user_version, each in its own
// transaction that also records its version, so a failed step leaves the
// database at the previous version. It returns the versions applied.
//
//	applied, err := db.Migrate(ctx, []bangdb.Migration{
//		{Version: 1, SQL: `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)`},
//		{Version: 2, SQL: `ALTER TABLE users ADD COLUMN email TEXT`},
//	})
func (db *DB) Migrate(ctx context.Context, steps []Migration) ([]int, error) {
	steps = append([]Migration(nil), steps...)
	sort.Slice(steps, func(i, j int) bool { return steps[i].Version < steps[j].Version })
	for i, m := range steps {
		if m.Version <= 0 || m.Version >= 1<<31 {
			return nil, fmt.Errorf("bangdb: migration %q: version %d must be between 1 and 2^31-1", m.Name, m.Version)
		}
		if i > 0 && steps[i-1].Version == m.Version {
			return nil, fmt.Errorf("bangdb: migration version %d is used twice", m.Version)
		}
	}
	var applied []int
	for _, m := range steps {
		done, err := db.migrateStep(ctx, m)
		if err != nil {
			return applied, err
		}
		if done {
			applied = append(applied, m.Version)
		}
	}
	return applied, nil
}

// migrateStep applies m if the database is older; the version is read in
// the step's transaction, so concurrent migrators cannot both apply it.
func (db *DB) migrateStep(ctx context.Context, m Migration) (bool, error) {
	tx, err := db.BeginContext(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.t.ExecContext(ctx, "PRAGMA user_version")
	if err != nil {
		return false, err
	}
	if cur := res.Rows[0][0].(int64); cur >= int64(m.Version) {
		return false, nil
	}
	fail := func(err error) (bool, error) {
		name := m.Name
		if name == "" {
			name = fmt.Sprint(m.Version)
		}
		return false, fmt.Errorf("bangdb: migration %s: %w", name, err)
	}
	if m.SQL != "" {
		if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
			return fail(err)
		}
	}
	if m.Func != nil {
		if err := m.Func(tx); err != nil {
			return fail(err)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.Version)); err != nil {
		return fail(err)
	}
	return true, tx.Commit()
}
