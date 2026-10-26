package store

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/knoxxr/bangdb/internal/btree"
	"github.com/knoxxr/bangdb/internal/pager"
	"github.com/knoxxr/bangdb/internal/value"
)

func init() { Register(TimeSeriesKind{}) }

// TimeSeriesKind stores rows in time order, compressed by column.
//
// A table names an INTEGER time column holding epoch time in time_unit
// (s, ms or us). Row ids are tick<<shift | seq, so rowid order is time
// order: time ranges become rowid ranges and ORDER BY time needs no sort.
//
// Everything lives in one B+tree (so WAL, backups, replication, VACUUM and
// the integrity check apply as usual) under three key spaces:
//
//	0x01 rowid(first)   sealed block: last rowid, row count, column-encoded
//	                    rows (tsblock.go)
//	0x02 rowid          single row (a recent row not yet sealed, or one that
//	                    overrides a sealed row), or a tombstone hiding a
//	                    sealed row
//	0x03                meta: counters (see tsMeta); last in key order, so
//	                    it shares the rightmost leaf with the newest rows
//	                    and an append changes one page
//
// New rows after the last block are kept as single rows; once block_rows
// of them have accumulated they are sealed into a block. Blocks never
// overlap. Changes to sealed rows are written as singles; once a block has
// collected a quarter of block_rows of them it is compacted: re-encoded
// with the changes applied (see compact). Rows older than retention
// (relative to the newest row) are dropped whole blocks at a time when a
// block is sealed.
type TimeSeriesKind struct{}

func (TimeSeriesKind) Name() string   { return "timeseries" }
func (TimeSeriesKind) Volatile() bool { return false }

func (TimeSeriesKind) Create(v *pager.View) (uint32, error) { return btree.Create(v) }

func (TimeSeriesKind) Drop(v *pager.View, root uint32) error { return btree.Drop(v, root) }

func (TimeSeriesKind) Open(v *pager.View, root uint32) TableStore {
	panic("store: time-series tables need their configuration (OpenConfigured)")
}

var errNoIndexes = errors.New("bangdb: time-series tables do not support indexes, PRIMARY KEY or UNIQUE (rows are located by time)")

func (TimeSeriesKind) CreateIndex(*pager.View) (uint32, error)  { return 0, errNoIndexes }
func (TimeSeriesKind) OpenIndex(*pager.View, uint32) IndexStore { panic(errNoIndexes) }
func (TimeSeriesKind) DropIndex(*pager.View, uint32) error      { return errNoIndexes }
func (TimeSeriesKind) OpenConfigured(v *pager.View, root uint32, cfg any) TableStore {
	return &tsStore{t: btree.Open(v, root), cfg: cfg.(*tsConfig)}
}

type tsConfig struct {
	timeCol   int
	unit      string
	shift     uint
	blockRows int
	retention int64 // ticks; 0 = keep everything
}

var unitShift = map[string]uint{"s": 20, "ms": 16, "us": 10}

// unitMicros is the length of one tick in microseconds.
var unitMicros = map[string]int64{"s": 1e6, "ms": 1e3, "us": 1}

// Configure validates the options: time_column (required), time_unit
// (s, ms or us; default s), block_rows (default 1024) and retention (an
// interval such as '30 days', or a number of ticks).
func (TimeSeriesKind) Configure(spec TableSpec) (any, error) {
	cfg := &tsConfig{timeCol: -1, unit: "s", blockRows: 1024}
	for k := range spec.Options {
		switch k {
		case "time_column", "time_unit", "block_rows", "retention":
		default:
			return nil, fmt.Errorf("bangdb: unknown time-series option %q", k)
		}
	}
	if spec.RowidColumn >= 0 {
		return nil, errNoIndexes
	}
	name := spec.Options["time_column"]
	for i, c := range spec.Columns {
		if strings.EqualFold(c.Name, name) {
			cfg.timeCol = i
		}
	}
	if cfg.timeCol < 0 {
		return nil, fmt.Errorf("bangdb: time-series tables need WITH (time_column = <an INTEGER column>)")
	}
	if spec.Columns[cfg.timeCol].Type != "INTEGER" {
		return nil, fmt.Errorf("bangdb: time column %s must be INTEGER (epoch time in time_unit)", name)
	}
	if u, ok := spec.Options["time_unit"]; ok {
		cfg.unit = strings.ToLower(u)
	}
	shift, ok := unitShift[cfg.unit]
	if !ok {
		return nil, fmt.Errorf("bangdb: time_unit must be s, ms or us")
	}
	cfg.shift = shift
	if s, ok := spec.Options["block_rows"]; ok {
		n, err := strconv.Atoi(s)
		if err != nil || n < 16 || n > 65536 {
			return nil, fmt.Errorf("bangdb: block_rows must be between 16 and 65536")
		}
		cfg.blockRows = n
	}
	if s, ok := spec.Options["retention"]; ok {
		ticks, err := ParseInterval(s, cfg.unit)
		if err != nil || ticks <= 0 {
			return nil, fmt.Errorf("bangdb: invalid retention %q (use e.g. '30 days' or a number of %s)", s, cfg.unit)
		}
		cfg.retention = ticks
	}
	return cfg, nil
}

// ParseInterval converts an interval such as "5 minutes", "1 day", "250ms"
// or a plain number (taken as ticks) to ticks of unit (s, ms or us).
func ParseInterval(s, unit string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, nil
	}
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	num, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, fmt.Errorf("bangdb: invalid interval %q", s)
	}
	var d time.Duration
	switch strings.TrimSuffix(strings.TrimSpace(s[i:]), "s") {
	case "u", "us", "microsecond":
		d = time.Microsecond
	case "m", "ms", "millisecond":
		if strings.TrimSpace(s[i:]) == "m" {
			d = time.Minute
		} else {
			d = time.Millisecond
		}
	case "", "sec", "second":
		d = time.Second
	case "min", "minute":
		d = time.Minute
	case "h", "hour":
		d = time.Hour
	case "d", "day":
		d = 24 * time.Hour
	case "w", "week":
		d = 7 * 24 * time.Hour
	default:
		return 0, fmt.Errorf("bangdb: invalid interval %q", s)
	}
	micros, ok := unitMicros[unit]
	if !ok {
		return 0, fmt.Errorf("bangdb: unknown time unit %q", unit)
	}
	ticks := num * float64(d/time.Microsecond) / float64(micros)
	if ticks < 1 || ticks > math.MaxInt64/2 {
		return 0, fmt.Errorf("bangdb: interval %q out of range", s)
	}
	return int64(ticks), nil
}

type tsStore struct {
	t   *btree.Tree
	cfg *tsConfig

	// The last block decoded by this handle (blocks change only through
	// this handle's sealing and DeleteRange, which reset it).
	cacheFirst int64
	cacheIDs   []int64
	cacheRows  [][]any
}

const (
	tsBlockKey  = 0x01
	tsSingleKey = 0x02
	tsMetaKey   = 0x03

	singleTomb = 0x00
	singleRow  = 0x01
)

// tsMeta holds the store's counters.
type tsMeta struct {
	hasBlock  bool
	blockLast int64 // last rowid of the last block
	tail      int   // single rows after blockLast (to be sealed)
	live      int64 // rows
	hasMax    bool
	max       int64 // upper bound of the rowids (exact unless the max row was deleted)
}

func key(space byte, rowid int64) []byte {
	return append([]byte{space}, value.EncodeRowID(rowid)...)
}

func (s *tsStore) meta() (tsMeta, error) {
	var m tsMeta
	data, ok, err := s.t.Get([]byte{tsMetaKey})
	if err != nil || !ok {
		return m, err
	}
	r := reader{b: data}
	m.hasBlock = r.byte() == 1
	m.blockLast = r.varint()
	m.tail = int(r.uvarint())
	m.live = r.varint()
	m.hasMax = r.byte() == 1
	m.max = r.varint()
	if r.err != nil {
		return m, errBadBlock
	}
	return m, nil
}

func (s *tsStore) setMeta(m tsMeta) error {
	b := []byte{boolByte(m.hasBlock)}
	b = binary.AppendVarint(b, m.blockLast)
	b = binary.AppendUvarint(b, uint64(m.tail))
	b = binary.AppendVarint(b, m.live)
	b = append(b, boolByte(m.hasMax))
	b = binary.AppendVarint(b, m.max)
	return s.t.Put([]byte{tsMetaKey}, b)
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}

// ---- blocks ----

type tsBlock struct {
	first, last int64
	count       int
	data        []byte // encoded rows
}

func parseBlock(k, v []byte) (tsBlock, error) {
	r := reader{b: v}
	b := tsBlock{first: value.DecodeRowID(k[1:]), last: r.varint(), count: int(r.uvarint())}
	b.data = r.b
	if r.err != nil || len(k) != 9 {
		return b, errBadBlock
	}
	return b, nil
}

func blockValue(last int64, count int, data []byte) []byte {
	b := binary.AppendVarint(nil, last)
	b = binary.AppendUvarint(b, uint64(count))
	return append(b, data...)
}

// decode returns the rows of b, using the handle's cache.
func (s *tsStore) decode(b tsBlock) ([]int64, [][]any, error) {
	if s.cacheIDs != nil && s.cacheFirst == b.first {
		return s.cacheIDs, s.cacheRows, nil
	}
	ids, rows, err := decodeBlock(b.data)
	if err != nil {
		return nil, nil, err
	}
	if len(ids) != b.count {
		return nil, nil, errBadBlock
	}
	s.cacheFirst, s.cacheIDs, s.cacheRows = b.first, ids, rows
	return ids, rows, nil
}

func (s *tsStore) dropCache() { s.cacheIDs, s.cacheRows = nil, nil }

// covering returns the block whose range contains rowid, if any.
func (s *tsStore) covering(rowid int64) (tsBlock, bool, error) {
	c := s.t.SeekReverse(key(tsBlockKey, rowid))
	if !c.Valid() {
		return tsBlock{}, false, c.Err()
	}
	if c.Key()[0] != tsBlockKey {
		return tsBlock{}, false, nil
	}
	v, err := c.Value()
	if err != nil {
		return tsBlock{}, false, err
	}
	b, err := parseBlock(c.Key(), v)
	if err != nil || b.last < rowid {
		return b, false, err
	}
	return b, true, nil
}

// blockRow returns the row of rowid stored in a sealed block.
func (s *tsStore) blockRow(rowid int64) ([]any, bool, error) {
	b, ok, err := s.covering(rowid)
	if err != nil || !ok {
		return nil, false, err
	}
	ids, rows, err := s.decode(b)
	if err != nil {
		return nil, false, err
	}
	i := sort.Search(len(ids), func(i int) bool { return ids[i] >= rowid })
	if i < len(ids) && ids[i] == rowid {
		return rows[i], true, nil
	}
	return nil, false, nil
}

// single returns the single entry for rowid: present, and whether it is a
// tombstone.
func (s *tsStore) single(rowid int64) (row []any, found, tomb bool, err error) {
	v, ok, err := s.t.Get(key(tsSingleKey, rowid))
	if err != nil || !ok {
		return nil, false, false, err
	}
	if len(v) == 0 || v[0] == singleTomb {
		return nil, true, true, nil
	}
	row, err = value.DecodeRow(v[1:])
	return row, true, false, err
}

// ---- TableStore ----

func (s *tsStore) Get(rowid int64) ([]any, bool, error) {
	row, found, tomb, err := s.single(rowid)
	if err != nil || found {
		return row, found && !tomb, err
	}
	row, ok, err := s.blockRow(rowid)
	if ok {
		row = append([]any(nil), row...) // the cached block must not change
	}
	return row, ok, err
}

func (s *tsStore) Put(rowid int64, row []any) error {
	m, err := s.meta()
	if err != nil {
		return err
	}
	inBlocks := m.hasBlock && rowid <= m.blockLast
	var found, tomb bool
	if m.hasMax && rowid <= m.max { // a newer rowid cannot exist yet
		if _, found, tomb, err = s.single(rowid); err != nil {
			return err
		}
	}
	existed := found && !tomb
	if !found && inBlocks {
		if _, existed, err = s.blockRow(rowid); err != nil {
			return err
		}
	}
	if err := s.t.Put(key(tsSingleKey, rowid), append([]byte{singleRow}, value.EncodeRow(row)...)); err != nil {
		return err
	}
	if !existed {
		m.live++
		if !inBlocks {
			m.tail++
		}
	}
	if !m.hasMax || rowid > m.max {
		m.hasMax, m.max = true, rowid
	}
	if inBlocks {
		if err := s.compact(rowid); err != nil {
			return err
		}
	}
	if m.tail >= s.cfg.blockRows {
		if err := s.seal(&m); err != nil {
			return err
		}
	}
	return s.setMeta(m)
}

func (s *tsStore) Delete(rowid int64) (bool, error) {
	m, err := s.meta()
	if err != nil {
		return false, err
	}
	_, found, tomb, err := s.single(rowid)
	if err != nil || found && tomb {
		return false, err
	}
	inBlock := false
	if m.hasBlock && rowid <= m.blockLast {
		if _, inBlock, err = s.blockRow(rowid); err != nil {
			return false, err
		}
	}
	switch {
	case inBlock: // hide the sealed row
		if err = s.t.Put(key(tsSingleKey, rowid), []byte{singleTomb}); err == nil {
			err = s.compact(rowid)
		}
	case found:
		_, err = s.t.Delete(key(tsSingleKey, rowid))
		if !m.hasBlock || rowid > m.blockLast {
			m.tail--
		}
	default:
		return false, nil
	}
	if err != nil {
		return false, err
	}
	m.live--
	return true, s.setMeta(m)
}

// compact rewrites the block containing rowid with the single rows in its
// range (overrides, tombstones and late rows) merged in, once there are
// enough of them, so that reads do not have to merge many entries and
// space is reclaimed.
func (s *tsStore) compact(rowid int64) error {
	b, ok, err := s.covering(rowid)
	if err != nil || !ok {
		return err
	}
	threshold := max(s.cfg.blockRows/4, 1)
	n := 0
	c := s.t.Seek(key(tsSingleKey, b.first))
	end := key(tsSingleKey, b.last)
	for ; c.Valid() && bytes.Compare(c.Key(), end) <= 0 && n < threshold; c.Next() {
		n++
	}
	if err := c.Err(); err != nil || n < threshold {
		return err
	}
	ids, rows, err := decodeBlock(b.data)
	if err != nil {
		return err
	}
	merged := make(map[int64][]any, len(ids))
	for i, id := range ids {
		merged[id] = rows[i]
	}
	var singles []int64
	c = s.t.Seek(key(tsSingleKey, b.first))
	for ; c.Valid() && bytes.Compare(c.Key(), end) <= 0; c.Next() {
		id := value.DecodeRowID(c.Key()[1:])
		singles = append(singles, id)
		v, err := c.Value()
		if err != nil {
			return err
		}
		if len(v) == 0 || v[0] == singleTomb {
			delete(merged, id)
			continue
		}
		row, err := value.DecodeRow(v[1:])
		if err != nil {
			return err
		}
		merged[id] = row
	}
	if err := c.Err(); err != nil {
		return err
	}
	for _, id := range singles {
		if _, err := s.t.Delete(key(tsSingleKey, id)); err != nil {
			return err
		}
	}
	if _, err := s.t.Delete(key(tsBlockKey, b.first)); err != nil {
		return err
	}
	s.dropCache()
	if len(merged) == 0 {
		return nil
	}
	ids = ids[:0]
	for id := range merged {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	rows = rows[:0]
	for _, id := range ids {
		rows = append(rows, merged[id])
	}
	// Rows added in the block's range can make it grow: split it into
	// blocks of block_rows. The last one keeps the old block's end.
	for i := 0; i < len(ids); i += s.cfg.blockRows {
		j := min(i+s.cfg.blockRows, len(ids))
		last := ids[j-1]
		if j == len(ids) {
			last = b.last
		}
		if err := s.t.Put(key(tsBlockKey, ids[i]), blockValue(last, j-i, encodeBlock(ids[i:j], rows[i:j]))); err != nil {
			return err
		}
	}
	return nil
}

// seal turns the single rows after the last block into a block, then
// applies the retention policy.
func (s *tsStore) seal(m *tsMeta) error {
	start := int64(math.MinInt64)
	if m.hasBlock {
		if m.blockLast == math.MaxInt64 {
			return nil
		}
		start = m.blockLast + 1
	}
	var ids []int64
	var rows [][]any
	c := s.t.Seek(key(tsSingleKey, start))
	for ; c.Valid() && c.Key()[0] == tsSingleKey; c.Next() {
		v, err := c.Value()
		if err != nil {
			return err
		}
		if len(v) == 0 || v[0] != singleRow {
			continue // cannot happen after the last block
		}
		row, err := value.DecodeRow(v[1:])
		if err != nil {
			return err
		}
		ids = append(ids, value.DecodeRowID(c.Key()[1:]))
		rows = append(rows, row)
	}
	if err := c.Err(); err != nil || len(ids) == 0 {
		return err
	}
	for _, id := range ids {
		if _, err := s.t.Delete(key(tsSingleKey, id)); err != nil {
			return err
		}
	}
	last := ids[len(ids)-1]
	if err := s.t.Put(key(tsBlockKey, ids[0]), blockValue(last, len(ids), encodeBlock(ids, rows))); err != nil {
		return err
	}
	m.hasBlock, m.blockLast, m.tail = true, last, 0
	s.dropCache()
	if s.cfg.retention > 0 && m.hasMax {
		cutTick := (m.max >> s.cfg.shift) - s.cfg.retention
		if cutTick > math.MinInt64>>s.cfg.shift {
			n, err := s.deleteRange(m, math.MinInt64, cutTick<<s.cfg.shift-1)
			if err != nil {
				return err
			}
			_ = n
		}
	}
	return nil
}

func (s *tsStore) Scan(lo, hi int64, desc bool, fn func(int64, []any) (bool, error)) error {
	if lo > hi {
		return nil
	}
	it, err := s.newMerge(lo, hi, desc)
	if err != nil {
		return err
	}
	for {
		id, row, ok, err := it.next()
		if err != nil || !ok {
			return err
		}
		if cont, err := fn(id, append([]any(nil), row...)); err != nil || !cont {
			return err
		}
	}
}

func (s *tsStore) Last() (int64, bool, error) {
	var last int64
	found := false
	err := s.Scan(math.MinInt64, math.MaxInt64, true, func(id int64, _ []any) (bool, error) {
		last, found = id, true
		return false, nil
	})
	return last, found, err
}

func (s *tsStore) EstimateRows() (int64, error) {
	m, err := s.meta()
	return m.live, err
}

// ---- time keys ----

func (s *tsStore) TimeColumn() int { return s.cfg.timeCol }

// AssignRowid gives a new row the next free id of its timestamp.
func (s *tsStore) AssignRowid(row []any) (int64, error) {
	tick, ok := row[s.cfg.timeCol].(int64)
	if !ok {
		return 0, fmt.Errorf("bangdb: the time column of a time-series table must be a non-NULL integer")
	}
	shift := s.cfg.shift
	if tick > math.MaxInt64>>shift || tick < math.MinInt64>>shift {
		return 0, fmt.Errorf("bangdb: time %d is out of range for time_unit %s", tick, s.cfg.unit)
	}
	base, mask := tick<<shift, int64(1)<<shift-1
	m, err := s.meta()
	if err != nil {
		return 0, err
	}
	if !m.hasMax || m.max>>shift < tick {
		return base, nil // the common case: newer than everything
	}
	if m.max>>shift == tick && m.max&mask < mask {
		return m.max + 1, nil
	}
	// Out of order: find the last row of this tick.
	last, found := int64(0), false
	err = s.Scan(base, base|mask, true, func(id int64, _ []any) (bool, error) {
		last, found = id, true
		return false, nil
	})
	switch {
	case err != nil:
		return 0, err
	case !found:
		return base, nil
	case last&mask == mask:
		return 0, fmt.Errorf("bangdb: too many rows with time %d (at most %d per %s)", tick, mask+1, s.cfg.unit)
	}
	return last + 1, nil
}

// RowidRange converts time bounds to rowid bounds.
func (s *tsStore) RowidRange(lo any, loInc bool, hi any, hiInc bool) (int64, int64, bool) {
	shift := s.cfg.shift
	minTick, maxTick := int64(math.MinInt64)>>shift, int64(math.MaxInt64)>>shift
	from, to := int64(math.MinInt64), int64(math.MaxInt64)
	if lo != nil {
		t, ok := tickBound(lo, true, loInc)
		if !ok {
			return 0, 0, false
		}
		if t > maxTick {
			return 1, 0, true // empty
		}
		if t > minTick {
			from = t << shift
		}
	}
	if hi != nil {
		t, ok := tickBound(hi, false, hiInc)
		if !ok {
			return 0, 0, false
		}
		if t < minTick {
			return 1, 0, true
		}
		if t < maxTick {
			to = t<<shift | (int64(1)<<shift - 1)
		}
	}
	return from, to, true
}

// tickBound returns the smallest (lower) or largest (upper) tick satisfying
// the bound.
func tickBound(v any, lower, inc bool) (int64, bool) {
	var f float64
	switch x := v.(type) {
	case int64:
		if !inc {
			if lower {
				if x == math.MaxInt64 {
					return x, true
				}
				return x + 1, true
			}
			if x == math.MinInt64 {
				return x, true
			}
			return x - 1, true
		}
		return x, true
	case float64:
		f = x
	default:
		return 0, false // other types do not compare with integers as numbers
	}
	if math.IsNaN(f) {
		return 0, false
	}
	if lower {
		c := math.Ceil(f)
		if c == f && !inc {
			c++
		}
		return clampTick(c), true
	}
	c := math.Floor(f)
	if c == f && !inc {
		c--
	}
	return clampTick(c), true
}

func clampTick(f float64) int64 {
	switch {
	case f >= math.MaxInt64:
		return math.MaxInt64
	case f <= math.MinInt64:
		return math.MinInt64
	}
	return int64(f)
}

// ---- range deletion ----

// DeleteRange deletes the rows with lo <= rowid <= hi: blocks inside the
// range are dropped whole.
func (s *tsStore) DeleteRange(lo, hi int64) (int64, error) {
	m, err := s.meta()
	if err != nil {
		return 0, err
	}
	n, err := s.deleteRange(&m, lo, hi)
	if err != nil {
		return 0, err
	}
	return n, s.setMeta(m)
}

func (s *tsStore) deleteRange(m *tsMeta, lo, hi int64) (int64, error) {
	if lo > hi {
		return 0, nil
	}
	// Count first (the merge applies overrides and tombstones).
	var n int64
	err := s.Scan(lo, hi, false, func(int64, []any) (bool, error) { n++; return true, nil })
	if err != nil || n == 0 && !s.hasEntries(lo, hi) {
		return 0, err
	}
	s.dropCache()
	// Blocks overlapping the range.
	var blocks []tsBlock
	start := key(tsBlockKey, lo)
	if b, ok, err := s.covering(lo); err != nil {
		return 0, err
	} else if ok {
		start = key(tsBlockKey, b.first)
	}
	c := s.t.Seek(start)
	for ; c.Valid() && c.Key()[0] == tsBlockKey; c.Next() {
		v, err := c.Value()
		if err != nil {
			return 0, err
		}
		b, err := parseBlock(c.Key(), v)
		if err != nil {
			return 0, err
		}
		if b.first > hi {
			break
		}
		b.data = append([]byte(nil), b.data...)
		blocks = append(blocks, b)
	}
	if err := c.Err(); err != nil {
		return 0, err
	}
	for _, b := range blocks {
		if _, err := s.t.Delete(key(tsBlockKey, b.first)); err != nil {
			return 0, err
		}
		if b.first >= lo && b.last <= hi {
			continue
		}
		// Keep the rows outside the range.
		ids, rows, err := decodeBlock(b.data)
		if err != nil {
			return 0, err
		}
		var kid []int64
		var krows [][]any
		for i, id := range ids {
			if id < lo || id > hi {
				kid, krows = append(kid, id), append(krows, rows[i])
			}
		}
		if len(kid) > 0 {
			if err := s.t.Put(key(tsBlockKey, kid[0]), blockValue(kid[len(kid)-1], len(kid), encodeBlock(kid, krows))); err != nil {
				return 0, err
			}
		}
	}
	// Singles (rows, overrides and tombstones) in the range.
	var singles [][]byte
	c = s.t.Seek(key(tsSingleKey, lo))
	for ; c.Valid() && c.Key()[0] == tsSingleKey && value.DecodeRowID(c.Key()[1:]) <= hi; c.Next() {
		singles = append(singles, append([]byte(nil), c.Key()...))
	}
	if err := c.Err(); err != nil {
		return 0, err
	}
	for _, k := range singles {
		if _, err := s.t.Delete(k); err != nil {
			return 0, err
		}
	}
	m.live -= n
	return n, s.recount(m)
}

func (s *tsStore) hasEntries(lo, hi int64) bool {
	c := s.t.Seek(key(tsSingleKey, lo))
	return c.Valid() && c.Key()[0] == tsSingleKey && value.DecodeRowID(c.Key()[1:]) <= hi
}

// recount restores blockLast, tail and max after a range deletion.
func (s *tsStore) recount(m *tsMeta) error {
	m.hasBlock = false
	c := s.t.SeekReverse(append([]byte{tsBlockKey}, bytes.Repeat([]byte{0xFF}, 8)...))
	if c.Valid() && c.Key()[0] == tsBlockKey {
		v, err := c.Value()
		if err != nil {
			return err
		}
		b, err := parseBlock(c.Key(), v)
		if err != nil {
			return err
		}
		m.hasBlock, m.blockLast = true, b.last
	}
	if err := c.Err(); err != nil {
		return err
	}
	start := int64(math.MinInt64)
	if m.hasBlock {
		start = m.blockLast + 1
	}
	m.tail = 0
	if !m.hasBlock || m.blockLast < math.MaxInt64 {
		c := s.t.Seek(key(tsSingleKey, start))
		for ; c.Valid() && c.Key()[0] == tsSingleKey; c.Next() {
			m.tail++
		}
		if err := c.Err(); err != nil {
			return err
		}
	}
	last, ok, err := s.Last()
	m.hasMax, m.max = ok, last
	return err
}

// ---- merged iteration ----

// tsMerge yields the rows of a rowid range from blocks and singles: a
// single replaces the block row with the same rowid, a tombstone hides it.
type tsMerge struct {
	s      *tsStore
	lo, hi int64
	desc   bool

	blocks *btree.Cursor    // ascending
	rblock *btree.RevCursor // descending
	ids    []int64          // rows of the current block
	rows   [][]any
	bi     int
	bdone  bool

	singles  *btree.Cursor
	rsingles *btree.RevCursor
}

func (s *tsStore) newMerge(lo, hi int64, desc bool) (*tsMerge, error) {
	it := &tsMerge{s: s, lo: lo, hi: hi, desc: desc}
	if desc {
		it.rblock = s.t.SeekReverse(key(tsBlockKey, hi))
		it.rsingles = s.t.SeekReverse(key(tsSingleKey, hi))
	} else {
		start := key(tsBlockKey, lo)
		if b, ok, err := s.covering(lo); err != nil {
			return nil, err
		} else if ok {
			start = key(tsBlockKey, b.first)
		}
		it.blocks = s.t.Seek(start)
		it.singles = s.t.Seek(key(tsSingleKey, lo))
	}
	return it, it.loadBlock()
}

// loadBlock positions on the next block row inside the range (or marks the
// blocks as done).
func (it *tsMerge) loadBlock() error {
	for {
		if it.bi >= 0 && it.bi < len(it.ids) {
			id := it.ids[it.bi]
			if !it.desc && id > it.hi || it.desc && id < it.lo {
				it.bdone = true
				return nil
			}
			if !it.desc && id >= it.lo || it.desc && id <= it.hi {
				return nil
			}
			it.advanceInBlock()
			continue
		}
		var k []byte
		var val func() ([]byte, error)
		var valid bool
		if it.desc {
			valid = it.rblock.Valid() && it.rblock.Key()[0] == tsBlockKey
			if valid {
				k, val = it.rblock.Key(), it.rblock.Value
			}
		} else {
			valid = it.blocks.Valid() && it.blocks.Key()[0] == tsBlockKey
			if valid {
				k, val = it.blocks.Key(), it.blocks.Value
			}
		}
		if !valid {
			it.bdone = true
			if it.desc {
				return it.rblock.Err()
			}
			return it.blocks.Err()
		}
		v, err := val()
		if err != nil {
			return err
		}
		b, err := parseBlock(k, v)
		if err != nil {
			return err
		}
		if it.desc {
			it.rblock.Prev()
		} else {
			it.blocks.Next()
		}
		if !it.desc && b.first > it.hi || it.desc && b.last < it.lo {
			it.bdone = true
			return nil
		}
		ids, rows, err := it.s.decode(b)
		if err != nil {
			return err
		}
		it.ids, it.rows = ids, rows
		if it.desc {
			it.bi = len(ids) - 1
		} else {
			it.bi = 0
		}
	}
}

func (it *tsMerge) advanceInBlock() {
	if it.desc {
		it.bi--
	} else {
		it.bi++
	}
}

// peekSingle returns the next single entry within the range.
func (it *tsMerge) peekSingle() (id int64, v func() ([]byte, error), ok bool) {
	if it.desc {
		c := it.rsingles
		if !c.Valid() || c.Key()[0] != tsSingleKey {
			return 0, nil, false
		}
		id = value.DecodeRowID(c.Key()[1:])
		return id, c.Value, id >= it.lo
	}
	c := it.singles
	if !c.Valid() || c.Key()[0] != tsSingleKey {
		return 0, nil, false
	}
	id = value.DecodeRowID(c.Key()[1:])
	return id, c.Value, id <= it.hi
}

func (it *tsMerge) nextSingle() {
	if it.desc {
		it.rsingles.Prev()
	} else {
		it.singles.Next()
	}
}

func (it *tsMerge) next() (int64, []any, bool, error) {
	for {
		sid, sval, sok := it.peekSingle()
		bok := !it.bdone
		if !sok && !bok {
			if it.desc {
				return 0, nil, false, it.rsingles.Err()
			}
			return 0, nil, false, it.singles.Err()
		}
		var bid int64
		if bok {
			bid = it.ids[it.bi]
		}
		before := func(a, b int64) bool { // a comes first in scan order
			if it.desc {
				return a > b
			}
			return a < b
		}
		if bok && (!sok || before(bid, sid)) {
			row := it.rows[it.bi]
			it.advanceInBlock()
			if err := it.loadBlock(); err != nil {
				return 0, nil, false, err
			}
			return bid, row, true, nil
		}
		// The single comes first, or replaces the block row.
		if bok && bid == sid {
			it.advanceInBlock()
			if err := it.loadBlock(); err != nil {
				return 0, nil, false, err
			}
		}
		v, err := sval()
		if err != nil {
			return 0, nil, false, err
		}
		it.nextSingle()
		if len(v) == 0 || v[0] == singleTomb {
			continue
		}
		row, err := value.DecodeRow(v[1:])
		if err != nil {
			return 0, nil, false, err
		}
		return sid, row, true, nil
	}
}
