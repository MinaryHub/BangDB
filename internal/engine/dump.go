package engine

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/knoxxr/bangdb/internal/sql"
	"github.com/knoxxr/bangdb/internal/store"
	"github.com/knoxxr/bangdb/internal/value"
)

// ---- dump ----

// Dump writes the database as an SQL script: tables (parents before the
// tables that reference them) with their rows, then indexes and views, in
// one transaction. It reads a snapshot, so writers are not blocked and the
// dump is consistent. Load it with ImportSQL (or by executing it).
func (db *DB) Dump(ctx context.Context, w io.Writer) error {
	bw := bufio.NewWriterSize(w, 64<<10)
	_, err := db.read(ctx, func(x *execCtx) (Result, error) { return Result{}, x.dump(bw) }, nil)
	if err != nil {
		return err
	}
	return bw.Flush()
}

func (x *execCtx) dump(w *bufio.Writer) error {
	tables := dumpOrder(x.cat)
	fmt.Fprintf(w, "-- BangDB dump\nBEGIN;\n")
	if uv := int32(x.v.Meta(metaUserVersion)); uv != 0 {
		fmt.Fprintf(w, "PRAGMA user_version = %d;\n", uv)
	}
	for _, t := range tables {
		fmt.Fprintf(w, "%s\n", t.createSQL())
		prefix := "INSERT INTO " + sql.QuoteIdent(t.Name) + " VALUES("
		stored := t.storedColumns() // generated columns are computed on import
		var last int64
		err := x.rows(t).Scan(store.MinRowid, store.MaxRowid, false, func(rowid int64, row []any) (bool, error) {
			if err := x.tick(); err != nil {
				return false, err
			}
			last = rowid
			w.WriteString(prefix)
			row = padRow(t, row)
			for i, c := range stored {
				if i > 0 {
					w.WriteByte(',')
				}
				w.WriteString(sqlLiteral(row[c]))
			}
			_, err := w.WriteString(");\n")
			return err == nil, err
		})
		if err != nil {
			return err
		}
		if t.autoIncrement() {
			// The AUTOINCREMENT high-water mark, as SQLite dumps it.
			hw, err := x.sequence(t)
			if err != nil {
				return err
			}
			if hw > last {
				fmt.Fprintf(w, "INSERT INTO sqlite_sequence VALUES(%s,%d);\n", sqlLiteral(t.Name), hw)
			}
		}
	}
	for _, t := range tables {
		for _, ix := range t.indexes {
			if !ix.Auto {
				fmt.Fprintf(w, "%s\n", ix.createSQL())
			}
		}
	}
	views := x.cat.ViewNames()
	sort.Strings(views)
	for _, v := range views {
		fmt.Fprintf(w, "%s\n", x.cat.DescribeView(v))
	}
	for _, tr := range x.cat.TriggerNames() {
		fmt.Fprintf(w, "%s\n", x.cat.DescribeTrigger(tr))
	}
	_, err := w.WriteString("COMMIT;\n")
	return err
}

// dumpOrder returns the tables sorted by name, except that a table comes
// after the tables its foreign keys reference.
func dumpOrder(c *Catalog) []*Table {
	var names []string
	for _, t := range c.tables {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	var out []*Table
	state := map[*Table]int{} // 1: visiting, 2: done
	var visit func(t *Table)
	visit = func(t *Table) {
		if state[t] != 0 {
			return // done, or a cycle (which CREATE TABLE cannot produce)
		}
		state[t] = 1
		for _, fk := range t.ForeignKeys {
			if p := c.table(fk.RefTable); p != nil && p != t {
				visit(p)
			}
		}
		state[t] = 2
		out = append(out, t)
	}
	for _, n := range names {
		visit(c.table(n))
	}
	return out
}

// sqlLiteral formats v as an SQL literal that reads back as the same value.
func sqlLiteral(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		switch {
		case math.IsInf(x, 1):
			return "9e999"
		case math.IsInf(x, -1):
			return "-9e999"
		case math.IsNaN(x):
			return "NULL"
		}
		s := strconv.FormatFloat(x, 'g', -1, 64)
		if !strings.ContainsAny(s, ".eEn") {
			s += ".0"
		}
		return s
	case string:
		return "'" + strings.ReplaceAll(x, "'", "''") + "'"
	case []byte:
		return "X'" + hex.EncodeToString(x) + "'"
	case value.Decimal:
		return "'" + x.String() + "'" // text keeps every digit; the column type converts it
	}
	return "'" + strings.ReplaceAll(value.Format(v), "'", "''") + "'"
}

// ---- SQL import ----

// ImportResult describes an import.
type ImportResult struct {
	Statements int      // statements executed
	Rows       int64    // rows inserted
	Skipped    []string // statements ignored (PRAGMA, triggers, ...), abbreviated
}

// ImportSQL runs an SQL script, such as the output of Dump or of SQLite's
// .dump, in a single transaction: either everything is imported or nothing.
//
// It accepts what SQLite dumps contain: BEGIN/COMMIT and PRAGMA statements
// are ignored, sqlite_sequence rows set AUTOINCREMENT high-water marks,
// tables may be created before the tables they reference, WITHOUT ROWID
// and STRICT are dropped, and triggers (not supported) are skipped and
// listed in the result. Foreign keys are checked once, at the end.
func (db *DB) ImportSQL(ctx context.Context, r io.Reader) (ImportResult, error) {
	var res ImportResult
	tx, err := db.BeginContext(ctx)
	if err != nil {
		return res, err
	}
	defer func() {
		if !tx.done {
			tx.Rollback()
		}
	}()
	tx.deferFK = true
	im := &importer{tx: tx, ctx: ctx, res: &res}
	sc := sql.NewScanner(r)
	for {
		text, err := sc.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return res, err
		}
		if err := im.statement(text); err != nil {
			return res, fmt.Errorf("bangdb: import: statement at line %d: %w", sc.Line(), err)
		}
	}
	if err := im.finish(); err != nil {
		return res, err
	}
	tx.deferFK = false
	if err := tx.checkForeignKeys(); err != nil {
		return res, err
	}
	return res, tx.Commit()
}

type importer struct {
	tx  *Tx
	ctx context.Context
	res *ImportResult
	// Statements waiting for a table: CREATE TABLEs whose referenced table
	// does not exist yet, with the statements that use them, and CREATE
	// VIEWs that failed (they are retried at the end).
	waiting  []*waitingTable
	views    []string
	triggers []string
}

type waitingTable struct {
	name  string   // the table being created (lower case)
	stmts []string // its CREATE TABLE, then statements on it
}

func (im *importer) statement(text string) error {
	words := sql.LeadingWords(text, 4)
	if len(words) == 0 {
		return nil
	}
	if words[0] == "PRAGMA" && len(words) > 1 && words[1] == "USER_VERSION" {
		_, err := im.exec(text)
		return err
	}
	switch words[0] {
	case "PRAGMA", "BEGIN", "COMMIT", "END", "ROLLBACK", "SAVEPOINT", "RELEASE", "ANALYZE":
		im.skip(text)
		return nil
	case "CREATE":
		k := 1
		if len(words) > 1 && (words[1] == "TEMP" || words[1] == "TEMPORARY") {
			k = 2
		}
		if len(words) > k && words[k] == "TRIGGER" {
			im.triggers = append(im.triggers, text) // after the tables and views
			return nil
		}
		if len(words) > k && words[k] == "VIEW" {
			if _, err := im.exec(text); err != nil {
				im.views = append(im.views, text) // may use a view created later
			}
			return nil
		}
		if len(words) > k && words[k] == "TABLE" {
			text = stripTableOptions(text)
		}
	}
	if target := statementTable(words); target != "" {
		if strings.HasPrefix(strings.ToLower(target), "sqlite_") {
			return im.sqliteTable(text)
		}
		if w := im.waitingFor(target); w != nil {
			w.stmts = append(w.stmts, text)
			return nil
		}
	}
	n, err := im.exec(text)
	var ref *unknownRefError
	if errors.As(err, &ref) && len(words) > 2 && words[1] == "TABLE" {
		// Created before the table it references: wait for that table.
		name := words[2]
		if name == "IF" {
			if w := sql.LeadingWords(text, 6); len(w) == 6 {
				name = w[5]
			}
		}
		im.waiting = append(im.waiting, &waitingTable{name: strings.ToLower(name), stmts: []string{text}})
		return nil
	}
	if err != nil {
		return err
	}
	im.res.Rows += n
	if len(words) > 1 && words[0] == "CREATE" && words[1] == "TABLE" {
		return im.retry()
	}
	return nil
}

func (im *importer) skip(text string) {
	s := strings.Join(strings.Fields(text), " ")
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	im.res.Skipped = append(im.res.Skipped, s)
}

// exec runs one statement and returns the number of rows it inserted.
func (im *importer) exec(text string) (int64, error) {
	stmts, names, err := sql.ParseNamed(text)
	if err != nil {
		return 0, err
	}
	if len(names) > 0 {
		return 0, errors.New("statement has parameters")
	}
	var n int64
	for _, s := range stmts {
		switch s.(type) {
		case *sql.Begin, *sql.Commit, *sql.Rollback, *sql.Vacuum:
			continue
		}
		if err := im.ctx.Err(); err != nil {
			return n, interrupted(err)
		}
		r, err := im.tx.execStmt(im.ctx, s, nil)
		if err != nil {
			return n, err
		}
		im.res.Statements++
		if _, ok := s.(*sql.Insert); ok {
			n += r.RowsAffected
		}
	}
	return n, nil
}

// waitingFor returns the waiting table named name, if any.
func (im *importer) waitingFor(name string) *waitingTable {
	name = strings.ToLower(name)
	for _, w := range im.waiting {
		if w.name == name {
			return w
		}
	}
	return nil
}

// retry runs the waiting tables whose referenced tables now exist.
func (im *importer) retry() error {
	for progress := true; progress; {
		progress = false
		for i := 0; i < len(im.waiting); i++ {
			w := im.waiting[i]
			_, err := im.exec(w.stmts[0])
			var ref *unknownRefError
			if errors.As(err, &ref) {
				continue
			}
			if err != nil {
				return err
			}
			im.waiting = append(im.waiting[:i], im.waiting[i+1:]...)
			i--
			progress = true
			for _, s := range w.stmts[1:] {
				n, err := im.exec(s)
				if err != nil {
					return err
				}
				im.res.Rows += n
			}
		}
	}
	return nil
}

func (im *importer) finish() error {
	if err := im.retry(); err != nil {
		return err
	}
	if len(im.waiting) > 0 {
		_, err := im.exec(im.waiting[0].stmts[0])
		return fmt.Errorf("bangdb: import: table %s: %w", im.waiting[0].name, err)
	}
	for len(im.views) > 0 {
		var failed []string
		var lastErr error
		for _, v := range im.views {
			if _, err := im.exec(v); err != nil {
				failed, lastErr = append(failed, v), err
			}
		}
		if len(failed) == len(im.views) {
			return fmt.Errorf("bangdb: import: %w", lastErr)
		}
		im.views = failed
	}
	// Triggers come last (the data is already in). One that uses
	// something BangDB lacks is skipped and reported, not fatal.
	for _, text := range im.triggers {
		if _, err := im.exec(text); err != nil {
			im.skip(text + " -- " + err.Error())
		}
	}
	return nil
}

// sqliteTable handles statements on SQLite's internal tables: rows of
// sqlite_sequence set AUTOINCREMENT high-water marks, the rest is skipped.
func (im *importer) sqliteTable(text string) error {
	stmts, _, err := sql.ParseNamed(text)
	if err != nil || len(stmts) != 1 {
		im.skip(text)
		return nil
	}
	ins, ok := stmts[0].(*sql.Insert)
	if !ok || !strings.EqualFold(ins.Table, "sqlite_sequence") || ins.Select != nil {
		im.skip(text)
		return nil
	}
	x := &execCtx{v: im.tx.v, cat: im.tx.cat, tx: im.tx, ctx: im.ctx}
	for _, row := range ins.Rows {
		if len(row) != 2 {
			return errors.New("sqlite_sequence rows have two values")
		}
		name, err := (&compiler{}).compile(row[0])
		if err != nil {
			return err
		}
		seq, err := (&compiler{}).compile(row[1])
		if err != nil {
			return err
		}
		nv, _ := name(&env{})
		sv, _ := seq(&env{})
		n, _ := nv.(string)
		hw, ok := sv.(int64)
		t := x.cat.table(n)
		if t == nil || !ok || !t.autoIncrement() {
			continue // a table that was skipped, or no AUTOINCREMENT here
		}
		if err := x.recordSequence(t, hw); err != nil {
			return err
		}
	}
	return nil
}

// statementTable returns the table an INSERT, DELETE, UPDATE or CREATE
// INDEX statement works on, from its leading words ("" if unknown).
func statementTable(words []string) string {
	switch {
	case len(words) >= 3 && words[0] == "INSERT" && words[1] == "INTO":
		return words[2]
	case len(words) >= 3 && words[0] == "DELETE" && words[1] == "FROM":
		return words[2]
	case len(words) >= 2 && words[0] == "UPDATE":
		return words[1]
	case len(words) >= 3 && words[0] == "CREATE" && words[1] == "TABLE" && words[2] != "IF":
		return words[2]
	}
	return ""
}

// stripTableOptions removes SQLite's table options (WITHOUT ROWID, STRICT)
// after the closing parenthesis of a CREATE TABLE.
func stripTableOptions(text string) string {
	i := strings.LastIndexByte(text, ')')
	if i < 0 {
		return text
	}
	tail := strings.ToUpper(strings.Join(strings.FieldsFunc(text[i+1:], func(r rune) bool {
		return r == ' ' || r == ',' || r == '\n' || r == '\t' || r == '\r'
	}), " "))
	switch tail {
	case "WITHOUT ROWID", "STRICT", "WITHOUT ROWID STRICT", "STRICT WITHOUT ROWID":
		return text[:i+1]
	}
	return text
}

// checkForeignKeys verifies every row of every table with foreign keys.
func (tx *Tx) checkForeignKeys() error {
	x := &execCtx{v: tx.v, cat: tx.cat, tx: tx, ctx: context.Background()}
	names := tx.cat.TableNames()
	sort.Strings(names)
	for _, n := range names {
		t := tx.cat.table(n)
		if len(t.ForeignKeys) == 0 {
			continue
		}
		var bad error
		err := x.rows(t).Scan(store.MinRowid, store.MaxRowid, false, func(rowid int64, row []any) (bool, error) {
			if err := x.checkParents(t, padRow(t, row), nil); err != nil {
				bad = fmt.Errorf("bangdb: import: table %s row %d: %w", t.Name, rowid, err)
				return false, nil
			}
			return true, nil
		})
		if err != nil {
			return err
		}
		if bad != nil {
			return bad
		}
	}
	return nil
}

// ---- CSV ----

// CSVOptions configures ImportCSV and ExportCSV.
type CSVOptions struct {
	Comma rune // field separator; default ','
	// NoHeader: the first record is data. By default it holds the column
	// names (ImportCSV maps fields to columns by name).
	NoHeader bool
	// Null is the field text that stands for NULL. The empty string
	// imports as NULL only when Null is "".
	Null string
	// Create makes ImportCSV create the table if it does not exist, with
	// a column per header field (type ANY: numbers become numbers).
	Create bool
}

// ImportCSV inserts the records of r into table in one transaction and
// returns the number of rows inserted. Fields are converted to numbers
// where the column type asks for it.
func (db *DB) ImportCSV(ctx context.Context, table string, r io.Reader, opts *CSVOptions) (int64, error) {
	if opts == nil {
		opts = &CSVOptions{}
	}
	cr := csv.NewReader(r)
	if opts.Comma != 0 {
		cr.Comma = opts.Comma
	}
	cr.FieldsPerRecord = -1
	cr.ReuseRecord = true
	tx, err := db.BeginContext(ctx)
	if err != nil {
		return 0, err
	}
	defer func() {
		if !tx.done {
			tx.Rollback()
		}
	}()
	var header []string
	if !opts.NoHeader {
		rec, err := cr.Read()
		if err != nil {
			if err == io.EOF {
				return 0, tx.Commit()
			}
			return 0, err
		}
		header = append([]string(nil), rec...)
		if len(header) > 0 {
			header[0] = strings.TrimPrefix(header[0], "\ufeff") // byte order mark
		}
	}
	if tx.cat.table(table) == nil && opts.Create {
		if header == nil {
			return 0, errors.New("bangdb: ImportCSV: Create needs a header")
		}
		cols := make([]string, len(header))
		for i, h := range header {
			cols[i] = sql.QuoteIdent(h)
		}
		if _, err := tx.ExecContext(ctx, "CREATE TABLE "+sql.QuoteIdent(table)+" ("+strings.Join(cols, ", ")+")"); err != nil {
			return 0, err
		}
	}
	t := tx.cat.table(table)
	if t == nil {
		return 0, fmt.Errorf("bangdb: no such table: %s", table)
	}
	var cols []string
	if header != nil {
		cols = header
	} else {
		for _, i := range t.storedColumns() {
			cols = append(cols, t.Columns[i].Name)
		}
	}
	quoted := make([]string, len(cols))
	marks := make([]string, len(cols))
	for i, c := range cols {
		if t.ColIndex(c) < 0 {
			return 0, fmt.Errorf("bangdb: ImportCSV: table %s has no column %s", t.Name, c)
		}
		quoted[i], marks[i] = sql.QuoteIdent(c), "?"
	}
	stmts, _, err := sql.ParseNamed("INSERT INTO " + sql.QuoteIdent(t.Name) + " (" + strings.Join(quoted, ", ") + ") VALUES (" + strings.Join(marks, ", ") + ")")
	if err != nil {
		return 0, err
	}
	ins := stmts[0]
	var n int64
	args := make([]any, len(cols))
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return n, err
		}
		if len(rec) != len(cols) {
			return n, fmt.Errorf("bangdb: ImportCSV: record %d has %d fields, expected %d", n+1, len(rec), len(cols))
		}
		for i, f := range rec {
			args[i] = csvValue(f, opts.Null, t.Columns[t.ColIndex(cols[i])].Type)
		}
		if _, err := tx.execStmt(ctx, ins, args); err != nil {
			return n, fmt.Errorf("bangdb: ImportCSV: record %d: %w", n+1, err)
		}
		n++
	}
	return n, tx.Commit()
}

// csvValue converts a CSV field for a column of type typ.
func csvValue(f, null string, typ value.Type) any {
	if f == null {
		return nil
	}
	switch typ {
	case value.Integer, value.Real, value.Any:
		if i, err := strconv.ParseInt(f, 10, 64); err == nil && (typ != value.Any || strconv.FormatInt(i, 10) == f) {
			return i // in an ANY column only canonical integers ("007" stays text)
		}
		digits := strings.TrimPrefix(f, "-")
		leadingZero := len(digits) > 1 && digits[0] == '0' && digits[1] != '.'
		if x, err := strconv.ParseFloat(f, 64); err == nil && strings.TrimSpace(f) == f && !strings.ContainsAny(f, "xXpP_") &&
			!(typ == value.Any && leadingZero) {
			if lower := strings.ToLower(f); !strings.Contains(lower, "inf") && !strings.Contains(lower, "nan") {
				return x
			}
		}
	}
	return f
}

// ExportCSV writes the result of a query as CSV (with a header unless
// opts.NoHeader) and returns the number of rows written. NULL is written as
// opts.Null, BLOBs as hex.
func (db *DB) ExportCSV(ctx context.Context, w io.Writer, opts *CSVOptions, query string, args ...any) (int64, error) {
	if opts == nil {
		opts = &CSVOptions{}
	}
	rows, err := db.QueryRows(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	cw := csv.NewWriter(w)
	if opts.Comma != 0 {
		cw.Comma = opts.Comma
	}
	if !opts.NoHeader {
		if err := cw.Write(rows.Columns()); err != nil {
			return 0, err
		}
	}
	var n int64
	rec := make([]string, len(rows.Columns()))
	for rows.Next() {
		for i, v := range rows.Row() {
			switch x := v.(type) {
			case nil:
				rec[i] = opts.Null
			case []byte:
				rec[i] = hex.EncodeToString(x)
			default:
				rec[i] = value.Format(x)
			}
		}
		if err := cw.Write(rec); err != nil {
			return n, err
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return n, err
	}
	cw.Flush()
	return n, cw.Error()
}

// Functions that SQLite dumps and scripts commonly use.
func init() {
	scalarFuncs["CHAR"] = scalarFunc{0, -1, func(a []any) (any, error) {
		var b strings.Builder
		for _, v := range a {
			n, ok := v.(int64)
			if !ok {
				if v == nil {
					continue
				}
				return nil, errors.New("bangdb: char() takes integer code points")
			}
			b.WriteRune(rune(n))
		}
		return b.String(), nil
	}, nil}
	scalarFuncs["UNICODE"] = scalarFunc{1, 1, func(a []any) (any, error) {
		if a[0] == nil {
			return nil, nil
		}
		for _, r := range value.Format(a[0]) {
			return int64(r), nil
		}
		return nil, nil
	}, nil}
	scalarFuncs["HEX"] = scalarFunc{1, 1, func(a []any) (any, error) {
		switch x := a[0].(type) {
		case nil:
			return "", nil
		case []byte:
			return strings.ToUpper(hex.EncodeToString(x)), nil
		default:
			return strings.ToUpper(hex.EncodeToString([]byte(value.Format(x)))), nil
		}
	}, nil}
	scalarFuncs["UNHEX"] = scalarFunc{1, 1, func(a []any) (any, error) {
		if a[0] == nil {
			return nil, nil
		}
		b, err := hex.DecodeString(value.Format(a[0]))
		if err != nil {
			return nil, nil
		}
		return b, nil
	}, nil}
}
