package store

import (
	"encoding/binary"
	"errors"
	"math"
	"math/bits"

	"github.com/minaryhub/bangdb/internal/value"
)

// Column-oriented block encoding for time-series data. A block holds the
// rows of a run of ascending rowids:
//
//	version(1) count(uvarint) ncols(uvarint) lengths(flag [uvarint...])
//	rowids: first(varint) then delta-of-delta (zigzag varints)
//	per column: encoding(1) [null bitmap] payload
//
// Column encodings pick what suits the values of the block: delta-of-delta
// or plain zigzag varints for integers, Gorilla XOR for floats, a
// dictionary for text, and generic row records for anything else (mixed
// types, blobs).

const blockVersion = 1

const (
	encNull    = 0 // every value NULL
	encIntDoD  = 1 // int64: delta-of-delta zigzag varints
	encIntRaw  = 2 // int64: zigzag varints
	encFloat   = 3 // float64: Gorilla XOR bit stream
	encDict    = 4 // string: dictionary + indices
	encGeneric = 5 // any values as a row record
)

var errBadBlock = errors.New("bangdb: corrupt time-series block")

// encodeBlock encodes rows (ascending rowids).
func encodeBlock(rowids []int64, rows [][]any) []byte {
	n := len(rows)
	ncols := 0
	sameLen := true
	for _, r := range rows {
		if len(r) != len(rows[0]) {
			sameLen = false
		}
		ncols = max(ncols, len(r))
	}
	b := []byte{blockVersion}
	b = binary.AppendUvarint(b, uint64(n))
	b = binary.AppendUvarint(b, uint64(ncols))
	if sameLen {
		b = append(b, 0)
	} else {
		b = append(b, 1)
		for _, r := range rows {
			b = binary.AppendUvarint(b, uint64(len(r)))
		}
	}
	b = appendDoD(b, rowids)
	col := make([]any, n)
	for c := 0; c < ncols; c++ {
		for i, r := range rows {
			col[i] = nil
			if c < len(r) {
				col[i] = r[c]
			}
		}
		b = appendColumn(b, col)
	}
	return b
}

func appendDoD(b []byte, vals []int64) []byte {
	var prev, delta int64
	for i, v := range vals {
		switch i {
		case 0:
			b = binary.AppendVarint(b, v)
		default:
			d := v - prev
			b = binary.AppendVarint(b, d-delta)
			delta = d
		}
		prev = v
	}
	return b
}

func appendColumn(b []byte, col []any) []byte {
	var kind byte = encNull
	nulls := 0
	for _, v := range col {
		var k byte
		switch v.(type) {
		case nil:
			nulls++
			continue
		case int64:
			k = encIntDoD
		case float64:
			k = encFloat
		case string:
			k = encDict
		default:
			k = encGeneric
		}
		if kind == encNull {
			kind = k
		} else if kind != k {
			kind = encGeneric
		}
	}
	if nulls == len(col) {
		return append(b, encNull)
	}
	if kind == encGeneric {
		enc := value.EncodeRow(col)
		b = append(b, encGeneric)
		return append(binary.AppendUvarint(b, uint64(len(enc))), enc...)
	}
	present := col
	var bitmap []byte
	if nulls > 0 {
		bitmap = make([]byte, (len(col)+7)/8)
		present = make([]any, 0, len(col)-nulls)
		for i, v := range col {
			if v != nil {
				bitmap[i/8] |= 1 << (i % 8)
				present = append(present, v)
			}
		}
	}
	switch kind {
	case encIntDoD:
		ints := make([]int64, len(present))
		for i, v := range present {
			ints[i] = v.(int64)
		}
		dod := appendDoD(nil, ints)
		var raw []byte
		for _, v := range ints {
			raw = binary.AppendVarint(raw, v)
		}
		if len(raw) < len(dod) {
			kind, dod = encIntRaw, raw
		}
		b = appendHeader(b, kind, bitmap)
		return append(b, dod...)
	case encFloat:
		b = appendHeader(b, kind, bitmap)
		var w bitWriter
		var prev uint64
		var lead, trail = -1, 0
		for i, v := range present {
			x := math.Float64bits(v.(float64))
			if i == 0 {
				w.write(x, 64)
				prev = x
				continue
			}
			xor := x ^ prev
			prev = x
			if xor == 0 {
				w.write(0, 1)
				continue
			}
			w.write(1, 1)
			l, t := bits.LeadingZeros64(xor), bits.TrailingZeros64(xor)
			if l > 31 {
				l = 31 // 5 bits
			}
			if lead >= 0 && l >= lead && t >= trail {
				// Fits in the previous window.
				w.write(0, 1)
				w.write(xor>>trail, uint(64-lead-trail))
				continue
			}
			lead, trail = l, t
			w.write(1, 1)
			w.write(uint64(l), 5)
			w.write(uint64(64-l-t-1), 6) // length 1..64 stored as 0..63
			w.write(xor>>t, uint(64-l-t))
		}
		return append(binary.AppendUvarint(b, uint64(len(w.buf))), w.buf...)
	default: // encDict
		b = appendHeader(b, kind, bitmap)
		index := map[string]int{}
		var dict []string
		idx := make([]int, len(present))
		for i, v := range present {
			s := v.(string)
			j, ok := index[s]
			if !ok {
				j = len(dict)
				index[s] = j
				dict = append(dict, s)
			}
			idx[i] = j
		}
		b = binary.AppendUvarint(b, uint64(len(dict)))
		for _, s := range dict {
			b = binary.AppendUvarint(b, uint64(len(s)))
			b = append(b, s...)
		}
		for _, j := range idx {
			b = binary.AppendUvarint(b, uint64(j))
		}
		return b
	}
}

func appendHeader(b []byte, kind byte, bitmap []byte) []byte {
	if bitmap == nil {
		return append(b, kind)
	}
	return append(append(b, kind|0x80), bitmap...)
}

// decodeBlock returns the rowids and rows of a block.
func decodeBlock(data []byte) ([]int64, [][]any, error) {
	r := reader{b: data}
	if r.byte() != blockVersion {
		return nil, nil, errBadBlock
	}
	// Every row and column takes at least one byte (or one bit): larger
	// counts can only come from a corrupt block.
	n := r.count(8 * len(data))
	ncols := r.count(len(data))
	if r.err != nil {
		return nil, nil, errBadBlock
	}
	lens := make([]int, n)
	if r.byte() == 1 {
		for i := range lens {
			lens[i] = r.count(ncols)
			if r.err != nil {
				return nil, nil, errBadBlock
			}
		}
	} else {
		for i := range lens {
			lens[i] = ncols
		}
	}
	rowids := r.dod(n)
	rows := make([][]any, n)
	for i := range rows {
		rows[i] = make([]any, lens[i])
	}
	for c := 0; c < ncols && r.err == nil; c++ {
		col, err := r.column(n)
		if err != nil {
			return nil, nil, err
		}
		for i := range rows {
			if c < lens[i] {
				rows[i][c] = col[i]
			}
		}
	}
	if r.err != nil {
		return nil, nil, errBadBlock
	}
	return rowids, rows, nil
}

type reader struct {
	b   []byte
	err error
}

func (r *reader) byte() byte {
	if len(r.b) == 0 {
		r.err = errBadBlock
		return 0
	}
	c := r.b[0]
	r.b = r.b[1:]
	return c
}

func (r *reader) uvarint() uint64 {
	v, k := binary.Uvarint(r.b)
	if k <= 0 {
		r.err = errBadBlock
		return 0
	}
	r.b = r.b[k:]
	return v
}

// count reads a length that must not exceed limit.
func (r *reader) count(limit int) int {
	v := r.uvarint()
	if v > uint64(limit) {
		r.err = errBadBlock
		return 0
	}
	return int(v)
}

func (r *reader) varint() int64 {
	v, k := binary.Varint(r.b)
	if k <= 0 {
		r.err = errBadBlock
		return 0
	}
	r.b = r.b[k:]
	return v
}

func (r *reader) bytes(n int) []byte {
	if n < 0 || n > len(r.b) {
		r.err = errBadBlock
		return nil
	}
	s := r.b[:n]
	r.b = r.b[n:]
	return s
}

func (r *reader) dod(n int) []int64 {
	out := make([]int64, n)
	var prev, delta int64
	for i := range out {
		v := r.varint()
		if i > 0 {
			delta += v
			v = prev + delta
		}
		out[i] = v
		prev = v
	}
	return out
}

func (r *reader) column(n int) ([]any, error) {
	col := make([]any, n)
	h := r.byte()
	kind := h &^ 0x80
	if kind == encNull {
		return col, r.err
	}
	if kind == encGeneric {
		vals, err := value.DecodeRow(r.bytes(r.count(len(r.b))))
		if err != nil || len(vals) != n {
			return nil, errBadBlock
		}
		return vals, r.err
	}
	present := n
	var bitmap []byte
	if h&0x80 != 0 {
		bitmap = r.bytes((n + 7) / 8)
		present = 0
		for _, c := range bitmap {
			present += bits.OnesCount8(c)
		}
	}
	vals := make([]any, present)
	switch kind {
	case encIntDoD:
		for i, v := range r.dod(present) {
			vals[i] = v
		}
	case encIntRaw:
		for i := range vals {
			vals[i] = r.varint()
		}
	case encFloat:
		br := bitReader{buf: r.bytes(r.count(len(r.b)))}
		var prev uint64
		lead, trail := 0, 0
		for i := range vals {
			if i == 0 {
				prev = br.read(64)
			} else if br.read(1) == 1 {
				if br.read(1) == 1 {
					lead = int(br.read(5))
					length := int(br.read(6)) + 1
					if trail = 64 - lead - length; trail < 0 {
						return nil, errBadBlock
					}
				}
				prev ^= br.read(uint(64-lead-trail)) << trail
			}
			vals[i] = math.Float64frombits(prev)
		}
		if br.err {
			return nil, errBadBlock
		}
	case encDict:
		dict := make([]string, r.count(len(r.b)))
		for i := range dict {
			dict[i] = string(r.bytes(r.count(len(r.b))))
		}
		for i := range vals {
			j := r.uvarint()
			if j >= uint64(len(dict)) {
				return nil, errBadBlock
			}
			vals[i] = dict[j]
		}
	default:
		return nil, errBadBlock
	}
	if bitmap == nil {
		copy(col, vals)
	} else {
		k := 0
		for i := range col {
			if bitmap[i/8]&(1<<(i%8)) != 0 {
				if k >= len(vals) {
					return nil, errBadBlock
				}
				col[i] = vals[k]
				k++
			}
		}
	}
	return col, r.err
}

type bitWriter struct {
	buf  []byte
	used uint // bits used in the last byte
}

func (w *bitWriter) write(v uint64, n uint) {
	for n > 0 {
		if w.used == 0 || w.used == 8 {
			w.buf = append(w.buf, 0)
			w.used = 0
		}
		take := min(n, 8-w.used)
		chunk := (v >> (n - take)) & (1<<take - 1)
		w.buf[len(w.buf)-1] |= byte(chunk << (8 - w.used - take))
		w.used += take
		n -= take
	}
}

type bitReader struct {
	buf []byte
	pos uint // bit position
	err bool
}

func (r *bitReader) read(n uint) uint64 {
	var v uint64
	for n > 0 {
		i := r.pos / 8
		if int(i) >= len(r.buf) {
			r.err = true
			return 0
		}
		off := r.pos % 8
		take := min(n, 8-off)
		chunk := uint64(r.buf[i]>>(8-off-take)) & (1<<take - 1)
		v = v<<take | chunk
		r.pos += take
		n -= take
	}
	return v
}
