// Package value defines SQL values, their comparison rules and their binary
// encodings (row records and order-preserving index keys).
//
// A value is one of the Go types: nil (NULL), int64, float64, string, []byte.
package value

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Type is a declared column type.
type Type string

const (
	Any     Type = "ANY"
	Integer Type = "INTEGER"
	Real    Type = "REAL"
	Text    Type = "TEXT"
	Blob    Type = "BLOB"
)

// ParseType maps a declared SQL type name to a storage type, using
// SQLite-like name rules (e.g. VARCHAR(20) -> TEXT, BIGINT -> INTEGER).
func ParseType(name string) Type {
	n := strings.ToUpper(name)
	switch {
	case n == "":
		return Any
	case strings.Contains(n, "INT"), strings.HasPrefix(n, "BOOL"):
		return Integer
	case strings.Contains(n, "CHAR"), strings.Contains(n, "CLOB"), strings.Contains(n, "TEXT"),
		strings.Contains(n, "DATE"), strings.Contains(n, "TIME"), strings.Contains(n, "UUID"), strings.Contains(n, "JSON"):
		return Text
	case strings.Contains(n, "BLOB"), strings.Contains(n, "BINARY"), strings.Contains(n, "BYTE"):
		return Blob
	case strings.Contains(n, "REAL"), strings.Contains(n, "FLOA"), strings.Contains(n, "DOUB"),
		strings.Contains(n, "NUM"), strings.Contains(n, "DEC"):
		return Real
	}
	return Any
}

// Normalize converts common Go types to canonical value types.
func Normalize(v any) (any, error) {
	switch x := v.(type) {
	case nil, int64, float64, string, []byte:
		return x, nil
	case int:
		return int64(x), nil
	case int8:
		return int64(x), nil
	case int16:
		return int64(x), nil
	case int32:
		return int64(x), nil
	case uint8:
		return int64(x), nil
	case uint16:
		return int64(x), nil
	case uint32:
		return int64(x), nil
	case uint:
		if uint64(x) > math.MaxInt64 {
			return nil, fmt.Errorf("bangdb: integer %d out of range", x)
		}
		return int64(x), nil
	case uint64:
		if x > math.MaxInt64 {
			return nil, fmt.Errorf("bangdb: integer %d out of range", x)
		}
		return int64(x), nil
	case float32:
		return float64(x), nil
	case bool:
		if x {
			return int64(1), nil
		}
		return int64(0), nil
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano), nil
	}
	return nil, fmt.Errorf("bangdb: unsupported argument type %T", v)
}

// ErrTypeMismatch is returned when a value cannot be stored in a column.
var ErrTypeMismatch = errors.New("type mismatch")

// Coerce converts v to the column type t, or fails if that would lose
// information.
func Coerce(v any, t Type) (any, error) {
	if v == nil || t == Any {
		return v, nil
	}
	switch t {
	case Integer:
		switch x := v.(type) {
		case int64:
			return x, nil
		case float64:
			if x == math.Trunc(x) && x >= math.MinInt64 && x < math.MaxInt64 {
				return int64(x), nil
			}
		case string:
			if i, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64); err == nil {
				return i, nil
			}
		}
	case Real:
		switch x := v.(type) {
		case int64:
			return float64(x), nil
		case float64:
			return x, nil
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil {
				return f, nil
			}
		}
	case Text:
		switch x := v.(type) {
		case string:
			return x, nil
		case int64, float64:
			return Format(x), nil
		}
	case Blob:
		switch x := v.(type) {
		case []byte:
			return x, nil
		case string:
			return []byte(x), nil
		}
	}
	return nil, fmt.Errorf("%w: cannot store %s value %s in %s column", ErrTypeMismatch, TypeOf(v), Format(v), t)
}

// TypeOf returns the storage type name of a value.
func TypeOf(v any) string {
	switch v.(type) {
	case nil:
		return "NULL"
	case int64:
		return "INTEGER"
	case float64:
		return "REAL"
	case string:
		return "TEXT"
	case []byte:
		return "BLOB"
	}
	return fmt.Sprintf("%T", v)
}

// Format renders a value for display.
func Format(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		// Plain decimal notation for ordinary magnitudes, exponent otherwise.
		if a := math.Abs(x); a == 0 || a >= 1e-6 && a < 1e15 {
			return strconv.FormatFloat(x, 'f', -1, 64)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case string:
		return x
	case []byte:
		return fmt.Sprintf("x'%x'", x)
	}
	return fmt.Sprint(v)
}

func rank(v any) int {
	switch v.(type) {
	case nil:
		return 0
	case int64, float64:
		return 1
	case string:
		return 2
	default:
		return 3
	}
}

// Compare defines a total order: NULL < numbers < text < blob. Integers and
// reals compare numerically.
func Compare(a, b any) int {
	ra, rb := rank(a), rank(b)
	if ra != rb {
		return cmpInt(ra, rb)
	}
	switch x := a.(type) {
	case nil:
		return 0
	case int64:
		if y, ok := b.(int64); ok {
			return cmpInt64(x, y)
		}
		return cmpFloat(float64(x), b.(float64))
	case float64:
		if y, ok := b.(int64); ok {
			return cmpFloat(x, float64(y))
		}
		return cmpFloat(x, b.(float64))
	case string:
		return strings.Compare(x, b.(string))
	case []byte:
		return bytes.Compare(x, b.([]byte))
	}
	return 0
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Truthy evaluates a value as a SQL boolean: NULL is unknown (nil, false).
func Truthy(v any) (bool, bool) {
	switch x := v.(type) {
	case nil:
		return false, false
	case int64:
		return x != 0, true
	case float64:
		return x != 0, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return err == nil && f != 0, true
	case []byte:
		return len(x) > 0, true
	}
	return false, true
}

// ---- Row encoding ----

const (
	tagNull  = 0
	tagInt   = 1
	tagFloat = 2
	tagText  = 3
	tagBlob  = 4
)

// EncodeRow serializes a row of values.
func EncodeRow(vals []any) []byte {
	b := binary.AppendUvarint(nil, uint64(len(vals)))
	for _, v := range vals {
		switch x := v.(type) {
		case nil:
			b = append(b, tagNull)
		case int64:
			b = append(b, tagInt)
			b = binary.AppendVarint(b, x)
		case float64:
			b = append(b, tagFloat)
			b = binary.BigEndian.AppendUint64(b, math.Float64bits(x))
		case string:
			b = append(b, tagText)
			b = binary.AppendUvarint(b, uint64(len(x)))
			b = append(b, x...)
		case []byte:
			b = append(b, tagBlob)
			b = binary.AppendUvarint(b, uint64(len(x)))
			b = append(b, x...)
		default:
			panic(fmt.Sprintf("value: cannot encode %T", v))
		}
	}
	return b
}

var errBadRow = errors.New("bangdb: corrupt row record")

// DecodeRow parses a row produced by EncodeRow.
func DecodeRow(b []byte) ([]any, error) {
	n, k := binary.Uvarint(b)
	if k <= 0 {
		return nil, errBadRow
	}
	b = b[k:]
	if n > uint64(len(b)) { // every value takes at least one byte
		return nil, errBadRow
	}
	vals := make([]any, 0, n)
	for i := uint64(0); i < n; i++ {
		if len(b) == 0 {
			return nil, errBadRow
		}
		tag := b[0]
		b = b[1:]
		switch tag {
		case tagNull:
			vals = append(vals, nil)
		case tagInt:
			x, k := binary.Varint(b)
			if k <= 0 {
				return nil, errBadRow
			}
			vals = append(vals, x)
			b = b[k:]
		case tagFloat:
			if len(b) < 8 {
				return nil, errBadRow
			}
			vals = append(vals, math.Float64frombits(binary.BigEndian.Uint64(b)))
			b = b[8:]
		case tagText, tagBlob:
			l, k := binary.Uvarint(b)
			if k <= 0 || uint64(len(b)-k) < l {
				return nil, errBadRow
			}
			data := b[k : k+int(l)]
			if tag == tagText {
				vals = append(vals, string(data))
			} else {
				vals = append(vals, append([]byte(nil), data...))
			}
			b = b[k+int(l):]
		default:
			return nil, errBadRow
		}
	}
	return vals, nil
}

// ---- Order-preserving key encoding ----

// Key tags are ordered so that byte-wise comparison matches Compare for
// values of the same column type.
const (
	keyNull  = 0x05
	keyInt   = 0x10
	keyFloat = 0x11
	keyText  = 0x20
	keyBlob  = 0x30
)

// MinNonNullKey is a prefix that sorts after every NULL key.
var MinNonNullKey = []byte{keyNull + 1}

// AppendKey appends an order-preserving, prefix-free encoding of v.
func AppendKey(b []byte, v any) []byte {
	switch x := v.(type) {
	case nil:
		return append(b, keyNull)
	case int64:
		b = append(b, keyInt)
		return binary.BigEndian.AppendUint64(b, uint64(x)^(1<<63))
	case float64:
		bits := math.Float64bits(x)
		if x == 0 {
			bits = 0 // treat -0 as +0
		}
		if bits&(1<<63) != 0 {
			bits = ^bits
		} else {
			bits |= 1 << 63
		}
		b = append(b, keyFloat)
		return binary.BigEndian.AppendUint64(b, bits)
	case string:
		b = append(b, keyText)
		return appendEscaped(b, []byte(x))
	case []byte:
		b = append(b, keyBlob)
		return appendEscaped(b, x)
	}
	panic(fmt.Sprintf("value: cannot key-encode %T", v))
}

// appendEscaped writes 0x00 as 0x00 0xFF and terminates with 0x00 0x01, so
// that the encoding is prefix-free and keeps lexicographic order.
func appendEscaped(b, data []byte) []byte {
	for _, c := range data {
		if c == 0 {
			b = append(b, 0, 0xFF)
		} else {
			b = append(b, c)
		}
	}
	return append(b, 0, 1)
}

// EncodeRowID encodes a rowid as an order-preserving 8-byte key.
func EncodeRowID(id int64) []byte {
	return binary.BigEndian.AppendUint64(nil, uint64(id)^(1<<63))
}

// DecodeRowID is the inverse of EncodeRowID.
func DecodeRowID(b []byte) int64 {
	return int64(binary.BigEndian.Uint64(b) ^ (1 << 63))
}

// DecodeKey decodes n values encoded by AppendKey from the start of b. ok
// is false if b does not hold n complete values.
func DecodeKey(b []byte, n int) (vals []any, ok bool) {
	vals = make([]any, 0, n)
	for len(vals) < n {
		if len(b) == 0 {
			return nil, false
		}
		tag := b[0]
		b = b[1:]
		switch tag {
		case keyNull:
			vals = append(vals, nil)
		case keyInt:
			if len(b) < 8 {
				return nil, false
			}
			vals = append(vals, int64(binary.BigEndian.Uint64(b)^(1<<63)))
			b = b[8:]
		case keyFloat:
			if len(b) < 8 {
				return nil, false
			}
			bits := binary.BigEndian.Uint64(b)
			if bits&(1<<63) != 0 {
				bits &^= 1 << 63
			} else {
				bits = ^bits
			}
			vals = append(vals, math.Float64frombits(bits))
			b = b[8:]
		case keyText, keyBlob:
			var data []byte
			end := false
			for i := 0; i+1 < len(b); i++ {
				if b[i] != 0 {
					data = append(data, b[i])
					continue
				}
				if b[i+1] == 1 {
					b, end = b[i+2:], true
					break
				}
				data = append(data, 0) // 00 FF
				i++
			}
			if !end {
				return nil, false
			}
			if tag == keyText {
				vals = append(vals, string(data))
			} else {
				if data == nil {
					data = []byte{}
				}
				vals = append(vals, data)
			}
		default:
			return nil, false
		}
	}
	return vals, true
}
