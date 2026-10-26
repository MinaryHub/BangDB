// Package store defines how the engine stores table rows and index entries,
// so that storage structures other than the B+tree (an in-memory row store,
// time-series chunks) can be added without changing the executor.
//
// A store kind creates, opens and drops stores whose state lives in pager
// pages reachable from one root page, recorded in the catalog. Everything
// therefore shares the pager's transactions, snapshots, WAL, checkpoints,
// backups and physical replication.
//
// The executor only uses these interfaces for row and index access. Two
// physical maintenance tasks still look at pages directly and require every
// kind to keep its pages in B+trees: the integrity check (structure and
// page accounting) and VACUUM (which rebuilds the trees).
package store

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"

	"github.com/knoxxr/bangdb/internal/pager"
)

// MinRowid and MaxRowid bound rowid ranges.
const (
	MinRowid = math.MinInt64
	MaxRowid = math.MaxInt64
)

// TableStore holds the rows of one table, keyed by rowid. Rows are slices of
// engine values (nil, int64, float64, string, []byte); a store returns rows
// as they were written (the engine fills in columns added later).
type TableStore interface {
	// Get returns the row with the given rowid.
	Get(rowid int64) ([]any, bool, error)
	// Put inserts or replaces a row.
	Put(rowid int64, row []any) error
	// Delete removes a row and reports whether it existed.
	Delete(rowid int64) (bool, error)
	// Scan visits the rows with lo <= rowid <= hi in ascending (or, with
	// desc, descending) rowid order until fn returns false or an error.
	Scan(lo, hi int64, desc bool, fn func(rowid int64, row []any) (bool, error)) error
	// Last returns the largest rowid.
	Last() (int64, bool, error)
	// EstimateRows returns an approximate row count, cheaply.
	EstimateRows() (int64, error)
}

// IndexStore holds the entries of one secondary index: byte keys (the
// order-preserving encoding of the indexed values followed by the rowid)
// without values, in byte order.
type IndexStore interface {
	Put(key []byte) error
	Delete(key []byte) (bool, error)
	// Seek returns an iterator over the keys >= start, ascending.
	Seek(start []byte) Iter
	// SeekReverse returns an iterator over the keys <= upper (nil = all),
	// descending.
	SeekReverse(upper []byte) Iter
	EstimateRows() (int64, error)
}

// Iter iterates over index keys. Key's result is valid until Next.
type Iter interface {
	Valid() bool
	Key() []byte
	Next()
	Err() error
}

// Kind is a way of storing tables.
type Kind interface {
	// Name is the identifier recorded in the catalog ("" is the default).
	Name() string
	// Create allocates an empty table store and returns its root page.
	Create(v *pager.View) (uint32, error)
	// Open returns the table store rooted at root, operating in v.
	Open(v *pager.View, root uint32) TableStore
	// Drop frees every page of the store.
	Drop(v *pager.View, root uint32) error
}

var (
	mu    sync.RWMutex
	kinds = map[string]Kind{}
)

// Register makes a kind available by name. It panics on duplicates.
func Register(k Kind) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := kinds[k.Name()]; dup {
		panic(fmt.Sprintf("store: kind %q registered twice", k.Name()))
	}
	kinds[k.Name()] = k
}

// ErrUnknownKind is returned for a kind name that is not registered.
var ErrUnknownKind = errors.New("unknown storage kind")

// Lookup returns the kind registered under name ("" = the B+tree row store).
func Lookup(name string) (Kind, error) {
	mu.RLock()
	defer mu.RUnlock()
	k, ok := kinds[name]
	if !ok {
		return nil, fmt.Errorf("bangdb: %w %q", ErrUnknownKind, name)
	}
	return k, nil
}

// Kinds returns the registered kind names, sorted.
func Kinds() []string {
	mu.RLock()
	defer mu.RUnlock()
	var out []string
	for n := range kinds {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
