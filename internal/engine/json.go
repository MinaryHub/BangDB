package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/minaryhub/bangdb/internal/sql"
	"github.com/minaryhub/bangdb/internal/value"
)

// JSON functions. JSON documents are TEXT values; they are parsed for each
// call into a small value model (nil, bool, json.Number, string, []any,
// *jsonObj) that keeps the order of object members.
//
// A TEXT argument of a JSON constructor (json_array, json_object, json_set,
// ...) is stored as a JSON string, unless it comes straight from another
// JSON function or the -> operator: such arguments are marked jsonText at
// compile time and embedded as JSON.

type jsonText string

type jsonObj struct {
	keys []string
	vals []any
}

func (o *jsonObj) index(k string) int {
	for i, key := range o.keys {
		if key == k {
			return i
		}
	}
	return -1
}

var errMalformedJSON = errors.New("bangdb: malformed JSON")

func parseJSON(s string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	v, err := decodeJSON(dec)
	if err != nil {
		return nil, errMalformedJSON
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errMalformedJSON
	}
	return v, nil
}

func decodeJSON(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '[':
			arr := []any{}
			for dec.More() {
				v, err := decodeJSON(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			_, err := dec.Token()
			return arr, err
		case '{':
			obj := &jsonObj{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, _ := kt.(string)
				v, err := decodeJSON(dec)
				if err != nil {
					return nil, err
				}
				if i := obj.index(k); i >= 0 {
					obj.vals[i] = v
				} else {
					obj.keys, obj.vals = append(obj.keys, k), append(obj.vals, v)
				}
			}
			_, err := dec.Token()
			return obj, err
		}
		return nil, errMalformedJSON
	default:
		return t, nil // nil, bool, json.Number, string
	}
}

func writeJSON(b *strings.Builder, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case json.Number:
		b.WriteString(string(x))
	case string:
		writeJSONString(b, x)
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSON(b, e)
		}
		b.WriteByte(']')
	case *jsonObj:
		b.WriteByte('{')
		for i, k := range x.keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSONString(b, k)
			b.WriteByte(':')
			writeJSON(b, x.vals[i])
		}
		b.WriteByte('}')
	}
}

func writeJSONString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' && c < utf8.RuneSelf {
			b.WriteByte(c)
			i++
			continue
		}
		switch c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if c < 0x20 {
				fmt.Fprintf(b, `\u%04x`, c)
			} else {
				r, n := utf8.DecodeRuneInString(s[i:])
				b.WriteRune(r) // invalid UTF-8 becomes U+FFFD
				i += n
				continue
			}
		}
		i++
	}
	b.WriteByte('"')
}

func jsonString(v any) string {
	var b strings.Builder
	writeJSON(&b, v)
	return b.String()
}

// toJSON converts an SQL value to a JSON value.
func toJSON(v any) (any, error) {
	switch x := v.(type) {
	case nil, string:
		return x, nil
	case jsonText:
		return parseJSON(string(x))
	case int64:
		return json.Number(strconv.FormatInt(x, 10)), nil
	case value.Decimal:
		return json.Number(x.String()), nil
	case float64:
		switch {
		case math.IsNaN(x):
			return nil, nil
		case math.IsInf(x, 1):
			return json.Number("9e999"), nil
		case math.IsInf(x, -1):
			return json.Number("-9e999"), nil
		}
		s := strconv.FormatFloat(x, 'g', -1, 64)
		if !strings.ContainsAny(s, ".e") {
			s += ".0"
		}
		return json.Number(s), nil
	case []byte:
		return nil, errors.New("bangdb: JSON cannot hold BLOB values")
	}
	return nil, fmt.Errorf("bangdb: unsupported value %T in JSON", v)
}

// fromJSON converts a JSON value to an SQL value: containers become JSON
// text, booleans 1 and 0.
func fromJSON(v any) any {
	switch x := v.(type) {
	case bool:
		if x {
			return int64(1)
		}
		return int64(0)
	case json.Number:
		if !strings.ContainsAny(string(x), ".eE") {
			if n, err := strconv.ParseInt(string(x), 10, 64); err == nil {
				return n
			}
		}
		f, _ := strconv.ParseFloat(string(x), 64)
		return f
	case []any, *jsonObj:
		return jsonString(x)
	}
	return v
}

func jsonTypeName(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		if x {
			return "true"
		}
		return "false"
	case json.Number:
		if _, ok := fromJSON(x).(int64); ok {
			return "integer"
		}
		return "real"
	case string:
		return "text"
	case []any:
		return "array"
	}
	return "object"
}

// jsonDoc parses a JSON document argument; ok is false for NULL.
func jsonDoc(v any) (doc any, ok bool, err error) {
	switch x := v.(type) {
	case nil:
		return nil, false, nil
	case string:
		doc, err = parseJSON(x)
	case jsonText:
		doc, err = parseJSON(string(x))
	case []byte:
		return nil, false, errors.New("bangdb: JSON cannot hold BLOB values")
	default:
		doc, err = toJSON(v) // a number is a valid JSON document
	}
	return doc, err == nil, err
}

// ---- paths ----

type pathStep struct {
	key     string
	isKey   bool
	idx     int64
	fromEnd bool // [#-n]: idx counts from the end; [#] is fromEnd with idx 0
}

func parsePath(p any) ([]pathStep, error) {
	s, ok := p.(string)
	if !ok {
		if t, isText := p.(jsonText); isText {
			s, ok = string(t), true
		}
	}
	if !ok || !strings.HasPrefix(s, "$") {
		return nil, fmt.Errorf("bangdb: bad JSON path: %s", formatPathArg(p))
	}
	bad := func() ([]pathStep, error) { return nil, fmt.Errorf("bangdb: bad JSON path: %q", s) }
	var steps []pathStep
	for i := 1; i < len(s); {
		switch s[i] {
		case '.':
			i++
			if i < len(s) && s[i] == '"' {
				j := strings.IndexByte(s[i+1:], '"')
				if j < 0 {
					return bad()
				}
				steps = append(steps, pathStep{key: s[i+1 : i+1+j], isKey: true})
				i += j + 2
				continue
			}
			j := i
			for j < len(s) && s[j] != '.' && s[j] != '[' {
				j++
			}
			if j == i {
				return bad()
			}
			steps = append(steps, pathStep{key: s[i:j], isKey: true})
			i = j
		case '[':
			j := strings.IndexByte(s[i:], ']')
			if j < 0 {
				return bad()
			}
			in := s[i+1 : i+j]
			st := pathStep{}
			if strings.HasPrefix(in, "#") {
				st.fromEnd = true
				in = strings.TrimSpace(in[1:])
				if in != "" {
					if in[0] != '-' {
						return bad()
					}
					in = strings.TrimSpace(in[1:])
				} else {
					in = "0"
				}
			}
			n, err := strconv.ParseInt(in, 10, 64)
			if err != nil || n < 0 {
				return bad()
			}
			st.idx = n
			steps = append(steps, st)
			i += j + 1
		default:
			return bad()
		}
	}
	return steps, nil
}

func formatPathArg(p any) string {
	if p == nil {
		return "NULL"
	}
	return fmt.Sprintf("%v", p)
}

// arrayIndex resolves an array step against an array of length n.
func (st pathStep) arrayIndex(n int) int64 {
	if st.fromEnd {
		return int64(n) - st.idx
	}
	return st.idx
}

func jsonLookup(doc any, steps []pathStep) (any, bool) {
	for _, st := range steps {
		switch x := doc.(type) {
		case *jsonObj:
			if !st.isKey {
				return nil, false
			}
			i := x.index(st.key)
			if i < 0 {
				return nil, false
			}
			doc = x.vals[i]
		case []any:
			if st.isKey {
				return nil, false
			}
			i := st.arrayIndex(len(x))
			if i < 0 || i >= int64(len(x)) {
				return nil, false
			}
			doc = x[i]
		default:
			return nil, false
		}
	}
	return doc, true
}

const (
	jsonSetMode     = iota // create or overwrite
	jsonInsertMode         // create only
	jsonReplaceMode        // overwrite only
)

// jsonEdit sets the value at steps in doc and returns the new document.
func jsonEdit(doc any, steps []pathStep, val any, mode int) any {
	if len(steps) == 0 {
		if mode == jsonInsertMode {
			return doc
		}
		return val
	}
	st, rest := steps[0], steps[1:]
	// The value placed where nothing exists: rest creates nested containers.
	fresh := func() (any, bool) {
		if mode == jsonReplaceMode {
			return nil, false
		}
		var v any = val
		for i := len(rest) - 1; i >= 0; i-- {
			r := rest[i]
			switch {
			case r.isKey:
				v = &jsonObj{keys: []string{r.key}, vals: []any{v}}
			case r.idx == 0: // [0] or [#] of a new array
				v = []any{v}
			default:
				return nil, false
			}
		}
		return v, true
	}
	switch x := doc.(type) {
	case *jsonObj:
		if !st.isKey {
			return doc
		}
		if i := x.index(st.key); i >= 0 {
			x.vals[i] = jsonEdit(x.vals[i], rest, val, mode)
		} else if v, ok := fresh(); ok {
			x.keys, x.vals = append(x.keys, st.key), append(x.vals, v)
		}
	case []any:
		if st.isKey {
			return doc
		}
		i := st.arrayIndex(len(x))
		switch {
		case i >= 0 && i < int64(len(x)):
			x[i] = jsonEdit(x[i], rest, val, mode)
		case i == int64(len(x)):
			if v, ok := fresh(); ok {
				return append(x, v)
			}
		}
	}
	return doc
}

func jsonRemove(doc any, steps []pathStep) any {
	if len(steps) == 0 {
		return nil
	}
	parent, ok := jsonLookup(doc, steps[:len(steps)-1])
	if !ok {
		return doc
	}
	st := steps[len(steps)-1]
	switch x := parent.(type) {
	case *jsonObj:
		if i := x.index(st.key); st.isKey && i >= 0 {
			x.keys = append(x.keys[:i], x.keys[i+1:]...)
			x.vals = append(x.vals[:i], x.vals[i+1:]...)
		}
	case []any:
		if i := st.arrayIndex(len(x)); !st.isKey && i >= 0 && i < int64(len(x)) {
			x = append(x[:i], x[i+1:]...)
			if len(steps) == 1 {
				return x
			}
			// Store the shortened array back into its parent.
			return jsonEdit(doc, steps[:len(steps)-1], x, jsonReplaceMode)
		}
	}
	return doc
}

// ---- SQL functions ----

// jsonProducing reports whether e yields JSON text that a JSON constructor
// should embed as JSON rather than as a string.
func jsonProducing(e sql.Expr) bool {
	switch x := e.(type) {
	case *sql.Func:
		switch x.Name {
		case "JSON", "JSON_ARRAY", "JSON_OBJECT", "JSON_SET", "JSON_INSERT", "JSON_REPLACE",
			"JSON_REMOVE", "JSON_PATCH", "JSON_GROUP_ARRAY", "JSON_GROUP_OBJECT":
			return true
		}
	case *sql.Binary:
		return x.Op == "->"
	}
	return false
}

// jsonValueArg reports whether argument i of function name is a JSON value
// (as opposed to a document, path or key).
func jsonValueArg(name string, i int) bool {
	switch name {
	case "JSON_ARRAY", "JSON_GROUP_ARRAY":
		return true
	case "JSON_OBJECT", "JSON_GROUP_OBJECT":
		return i%2 == 1
	case "JSON_SET", "JSON_INSERT", "JSON_REPLACE":
		return i > 0 && i%2 == 0
	case "JSON_PATCH":
		return i == 1
	}
	return false
}

func asJSONText(fn evalFn) evalFn {
	return func(e *env) (any, error) {
		v, err := fn(e)
		if s, ok := v.(string); ok {
			return jsonText(s), err
		}
		return v, err
	}
}

func init() {
	for name, f := range map[string]scalarFunc{
		"JSON":              {1, 1, fnJSON, nil},
		"JSON_VALID":        {1, 1, fnJSONValid, nil},
		"JSON_TYPE":         {1, 2, fnJSONType, nil},
		"JSON_EXTRACT":      {2, -1, fnJSONExtract, nil},
		"JSON_ARRAY_LENGTH": {1, 2, fnJSONArrayLength, nil},
		"JSON_ARRAY":        {0, -1, fnJSONArray, nil},
		"JSON_OBJECT":       {0, -1, fnJSONObject, nil},
		"JSON_SET":          {1, -1, func(a []any) (any, error) { return fnJSONEdit(a, jsonSetMode) }, nil},
		"JSON_INSERT":       {1, -1, func(a []any) (any, error) { return fnJSONEdit(a, jsonInsertMode) }, nil},
		"JSON_REPLACE":      {1, -1, func(a []any) (any, error) { return fnJSONEdit(a, jsonReplaceMode) }, nil},
		"JSON_REMOVE":       {1, -1, fnJSONRemove, nil},
		"JSON_PATCH":        {2, 2, fnJSONPatch, nil},
		"JSON_QUOTE":        {1, 1, fnJSONQuote, nil},
	} {
		scalarFuncs[name] = f
	}
}

func fnJSON(a []any) (any, error) {
	doc, ok, err := jsonDoc(a[0])
	if !ok {
		return nil, err
	}
	return jsonString(doc), nil
}

func fnJSONValid(a []any) (any, error) {
	switch x := a[0].(type) {
	case nil:
		return nil, nil
	case string:
		_, err := parseJSON(x)
		return boolVal(err == nil), nil
	case jsonText:
		return int64(1), nil
	case int64, float64:
		return int64(1), nil
	}
	return int64(0), nil
}

// jsonAt parses a[0] and looks up the optional path a[1].
func jsonAt(a []any) (any, bool, error) {
	doc, ok, err := jsonDoc(a[0])
	if !ok {
		return nil, false, err
	}
	if len(a) < 2 {
		return doc, true, nil
	}
	if a[1] == nil {
		return nil, false, nil
	}
	steps, err := parsePath(a[1])
	if err != nil {
		return nil, false, err
	}
	v, found := jsonLookup(doc, steps)
	return v, found, nil
}

func fnJSONType(a []any) (any, error) {
	v, ok, err := jsonAt(a)
	if !ok {
		return nil, err
	}
	return jsonTypeName(v), nil
}

func fnJSONArrayLength(a []any) (any, error) {
	v, ok, err := jsonAt(a)
	if !ok {
		return nil, err
	}
	if arr, isArr := v.([]any); isArr {
		return int64(len(arr)), nil
	}
	return int64(0), nil
}

func fnJSONExtract(a []any) (any, error) {
	doc, ok, err := jsonDoc(a[0])
	if !ok {
		return nil, err
	}
	var out []any
	for _, p := range a[1:] {
		if p == nil {
			return nil, nil
		}
		steps, err := parsePath(p)
		if err != nil {
			return nil, err
		}
		v, found := jsonLookup(doc, steps)
		if len(a) == 2 {
			if !found {
				return nil, nil
			}
			return fromJSON(v), nil
		}
		out = append(out, v) // missing paths become null
	}
	return jsonString(out), nil
}

// arrowPath turns the right operand of -> and ->> into a path: a full path
// ('$...'), an object label, or an array index (negative counts from the
// end).
func arrowPath(p any) ([]pathStep, error) {
	switch x := p.(type) {
	case string:
		if strings.HasPrefix(x, "$") {
			return parsePath(x)
		}
		return []pathStep{{key: x, isKey: true}}, nil
	case int64:
		if x < 0 {
			return []pathStep{{idx: -x, fromEnd: true}}, nil
		}
		return []pathStep{{idx: x}}, nil
	}
	return nil, fmt.Errorf("bangdb: bad JSON path: %s", formatPathArg(p))
}

// jsonArrow implements l -> r (asJSON) and l ->> r.
func jsonArrow(l, r any, asJSON bool) (any, error) {
	if l == nil || r == nil {
		return nil, nil
	}
	doc, ok, err := jsonDoc(l)
	if !ok {
		return nil, err
	}
	steps, err := arrowPath(r)
	if err != nil {
		return nil, err
	}
	v, found := jsonLookup(doc, steps)
	if !found {
		return nil, nil
	}
	if asJSON {
		return jsonString(v), nil
	}
	return fromJSON(v), nil
}

func fnJSONArray(a []any) (any, error) {
	arr := make([]any, len(a))
	for i, v := range a {
		j, err := toJSON(v)
		if err != nil {
			return nil, err
		}
		arr[i] = j
	}
	return jsonString(arr), nil
}

func fnJSONObject(a []any) (any, error) {
	if len(a)%2 != 0 {
		return nil, errors.New("bangdb: json_object() requires an even number of arguments")
	}
	obj := &jsonObj{}
	for i := 0; i < len(a); i += 2 {
		k, ok := a[i].(string)
		if !ok {
			return nil, errors.New("bangdb: json_object() labels must be TEXT")
		}
		v, err := toJSON(a[i+1])
		if err != nil {
			return nil, err
		}
		if j := obj.index(k); j >= 0 {
			obj.vals[j] = v
		} else {
			obj.keys, obj.vals = append(obj.keys, k), append(obj.vals, v)
		}
	}
	return jsonString(obj), nil
}

func fnJSONEdit(a []any, mode int) (any, error) {
	if len(a)%2 != 1 {
		return nil, errors.New("bangdb: json_set(), json_insert() and json_replace() take a document and path/value pairs")
	}
	doc, ok, err := jsonDoc(a[0])
	if !ok {
		return nil, err
	}
	for i := 1; i < len(a); i += 2 {
		if a[i] == nil {
			return nil, nil
		}
		steps, err := parsePath(a[i])
		if err != nil {
			return nil, err
		}
		v, err := toJSON(a[i+1])
		if err != nil {
			return nil, err
		}
		doc = jsonEdit(doc, steps, v, mode)
	}
	return jsonString(doc), nil
}

func fnJSONRemove(a []any) (any, error) {
	doc, ok, err := jsonDoc(a[0])
	if !ok {
		return nil, err
	}
	for _, p := range a[1:] {
		if p == nil {
			return nil, nil
		}
		steps, err := parsePath(p)
		if err != nil {
			return nil, err
		}
		if len(steps) == 0 {
			return nil, nil
		}
		doc = jsonRemove(doc, steps)
	}
	return jsonString(doc), nil
}

// fnJSONPatch applies an RFC 7396 merge patch.
func fnJSONPatch(a []any) (any, error) {
	doc, ok, err := jsonDoc(a[0])
	if !ok {
		return nil, err
	}
	patch, ok, err := jsonDoc(a[1])
	if !ok {
		return nil, err
	}
	return jsonString(mergePatch(doc, patch)), nil
}

func mergePatch(target, patch any) any {
	p, ok := patch.(*jsonObj)
	if !ok {
		return patch
	}
	t, ok := target.(*jsonObj)
	if !ok {
		t = &jsonObj{}
	}
	for i, k := range p.keys {
		j := t.index(k)
		switch {
		case p.vals[i] == nil:
			if j >= 0 {
				t.keys = append(t.keys[:j], t.keys[j+1:]...)
				t.vals = append(t.vals[:j], t.vals[j+1:]...)
			}
		case j >= 0:
			t.vals[j] = mergePatch(t.vals[j], p.vals[i])
		default:
			t.keys, t.vals = append(t.keys, k), append(t.vals, mergePatch(nil, p.vals[i]))
		}
	}
	return t
}

func fnJSONQuote(a []any) (any, error) {
	v, err := toJSON(a[0])
	if err != nil {
		return nil, err
	}
	return jsonString(v), nil
}

// ---- aggregates ----

// jsonPair is the argument of json_group_object.
type jsonPair struct{ k, v any }

func (st *aggState) stepJSON(v any) error {
	switch st.spec.name {
	case "JSON_GROUP_ARRAY":
		j, err := toJSON(v)
		if err != nil {
			return err
		}
		st.items = append(st.items, j)
	case "JSON_GROUP_OBJECT":
		p := v.(jsonPair)
		k, ok := p.k.(string)
		if !ok {
			if p.k == nil {
				return errors.New("bangdb: json_group_object() labels must not be NULL")
			}
			return errors.New("bangdb: json_group_object() labels must be TEXT")
		}
		j, err := toJSON(p.v)
		if err != nil {
			return err
		}
		if st.obj == nil {
			st.obj = &jsonObj{}
		}
		if i := st.obj.index(k); i >= 0 {
			st.obj.vals[i] = j
		} else {
			st.obj.keys, st.obj.vals = append(st.obj.keys, k), append(st.obj.vals, j)
		}
	}
	return nil
}

func (st *aggState) resultJSON() any {
	if st.spec.name == "JSON_GROUP_ARRAY" {
		if st.items == nil {
			return "[]"
		}
		return jsonString(st.items)
	}
	if st.obj == nil {
		return "{}"
	}
	return jsonString(st.obj)
}
