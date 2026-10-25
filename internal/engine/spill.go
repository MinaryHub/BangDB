package engine

import (
	"bufio"
	"container/heap"
	"encoding/binary"
	"io"
	"os"
	"sort"

	"github.com/knoxxr/bangdb/internal/value"
)

// DefaultSortMemory is the memory a query may use for sorting, DISTINCT and
// GROUP BY before it spills to temporary files.
const DefaultSortMemory = 64 << 20

// spillConfig is the per-database spill setting.
type spillConfig struct {
	limit int    // bytes; <= 0 never spills
	dir   string // "" = os.TempDir()
}

var defaultSpill = &spillConfig{limit: DefaultSortMemory}

func (x *execCtx) spillCfg() *spillConfig {
	if x.spill != nil {
		return x.spill
	}
	return defaultSpill
}

// rowBytes estimates the memory used by a row of values.
func rowBytes(row []any) int {
	n := 24 + 16*len(row)
	for _, v := range row {
		switch v := v.(type) {
		case string:
			n += len(v)
		case []byte:
			n += len(v)
		}
	}
	return n
}

// extSorter sorts records ([]any) of any number with bounded memory: when
// the buffered records exceed the limit they are sorted and written to a
// temporary file as a run; Finish merges the runs. Equal records keep their
// insertion order.
type extSorter struct {
	less  func(a, b []any) bool
	cfg   *spillConfig
	mem   [][]any
	bytes int
	file  *os.File
	runs  []run
	size  int64 // bytes written to file
}

type run struct{ off, n int64 }

func newSorter(cfg *spillConfig, less func(a, b []any) bool) *extSorter {
	return &extSorter{cfg: cfg, less: less}
}

func (s *extSorter) Add(rec []any) error {
	s.mem = append(s.mem, rec)
	s.bytes += rowBytes(rec)
	if s.cfg.limit > 0 && s.bytes > s.cfg.limit {
		return s.spill()
	}
	return nil
}

// Spilled reports whether any run went to disk.
func (s *extSorter) Spilled() bool { return len(s.runs) > 0 }

func (s *extSorter) sortMem() {
	sort.SliceStable(s.mem, func(i, j int) bool { return s.less(s.mem[i], s.mem[j]) })
}

func (s *extSorter) spill() error {
	if s.file == nil {
		f, err := os.CreateTemp(s.cfg.dir, "bangdb-sort-*")
		if err != nil {
			return err
		}
		os.Remove(f.Name()) // unlinked: the space is freed when it is closed (Unix)
		s.file = f
	}
	s.sortMem()
	w := bufio.NewWriterSize(io.NewOffsetWriter(s.file, s.size), 1<<16)
	start := s.size
	var lenBuf [binary.MaxVarintLen64]byte
	for _, rec := range s.mem {
		data := value.EncodeRow(rec)
		n := binary.PutUvarint(lenBuf[:], uint64(len(data)))
		if _, err := w.Write(lenBuf[:n]); err != nil {
			return err
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
		s.size += int64(n + len(data))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	s.runs = append(s.runs, run{start, s.size - start})
	s.mem, s.bytes = nil, 0
	return nil
}

// Finish returns an iterator over the sorted records. Close must be called.
func (s *extSorter) Finish() (*sortIter, error) {
	if len(s.runs) == 0 {
		s.sortMem()
		return &sortIter{mem: s.mem, s: s}, nil
	}
	if len(s.mem) > 0 {
		if err := s.spill(); err != nil {
			return nil, err
		}
	}
	it := &sortIter{s: s, h: &runHeap{less: s.less}}
	for i, r := range s.runs {
		rd := &runReader{r: bufio.NewReaderSize(io.NewSectionReader(s.file, r.off, r.n), 1<<16), idx: i}
		if err := rd.next(); err != nil {
			return nil, err
		}
		if rd.cur != nil {
			it.h.items = append(it.h.items, rd)
		}
	}
	heap.Init(it.h)
	return it, nil
}

// Close releases the temporary file.
func (s *extSorter) Close() {
	if s.file != nil {
		s.file.Close()
		os.Remove(s.file.Name())
		s.file = nil
	}
	s.mem = nil
}

type runReader struct {
	r   *bufio.Reader
	idx int
	cur []any
}

func (r *runReader) next() error {
	n, err := binary.ReadUvarint(r.r)
	if err == io.EOF {
		r.cur = nil
		return nil
	}
	if err != nil {
		return err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r.r, buf); err != nil {
		return err
	}
	if r.cur, err = value.DecodeRow(buf); err != nil {
		return err
	}
	if r.cur == nil {
		r.cur = []any{}
	}
	return nil
}

type runHeap struct {
	items []*runReader
	less  func(a, b []any) bool
}

func (h *runHeap) Len() int { return len(h.items) }
func (h *runHeap) Less(i, j int) bool {
	a, b := h.items[i], h.items[j]
	if h.less(a.cur, b.cur) {
		return true
	}
	if h.less(b.cur, a.cur) {
		return false
	}
	return a.idx < b.idx // earlier runs hold earlier records
}
func (h *runHeap) Swap(i, j int) { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *runHeap) Push(x any)    { h.items = append(h.items, x.(*runReader)) }
func (h *runHeap) Pop() any {
	x := h.items[len(h.items)-1]
	h.items = h.items[:len(h.items)-1]
	return x
}

type sortIter struct {
	s   *extSorter
	mem [][]any
	i   int
	h   *runHeap
}

// Next returns the next record, or nil at the end.
func (it *sortIter) Next() ([]any, error) {
	if it.h == nil {
		if it.i >= len(it.mem) {
			return nil, nil
		}
		it.i++
		return it.mem[it.i-1], nil
	}
	if it.h.Len() == 0 {
		return nil, nil
	}
	top := it.h.items[0]
	rec := top.cur
	if err := top.next(); err != nil {
		return nil, err
	}
	if top.cur == nil {
		heap.Pop(it.h)
	} else {
		heap.Fix(it.h, 0)
	}
	return rec, nil
}
