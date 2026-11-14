package main

import (
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"

	"github.com/knoxxr/bangdb/internal/engine"
	"github.com/knoxxr/bangdb/internal/value"
)

// The shell's output modes (.mode).
var outputModes = []string{"table", "list", "csv", "json", "line"}

// printResult writes a query result in the given mode:
//
//	table  aligned columns with a header and a row count (the default)
//	list   values separated by "|", one row per line
//	csv    comma-separated values (NULL is empty, a BLOB is hex)
//	json   an array of objects
//	line   one "column = value" per line, rows separated by an empty line
//
// headers adds the column names as the first line of list and csv.
func printResult(w io.Writer, res engine.Result, mode string, headers bool) error {
	switch mode {
	case "list":
		if headers {
			fmt.Fprintln(w, strings.Join(res.Columns, "|"))
		}
		for _, row := range res.Rows {
			vals := make([]string, len(row))
			for i, v := range row {
				vals[i] = value.Format(v)
			}
			fmt.Fprintln(w, strings.Join(vals, "|"))
		}
	case "csv":
		cw := csv.NewWriter(w)
		if headers {
			cw.Write(res.Columns)
		}
		rec := make([]string, len(res.Columns))
		for _, row := range res.Rows {
			for i, v := range row {
				switch x := v.(type) {
				case nil:
					rec[i] = ""
				case []byte:
					rec[i] = hex.EncodeToString(x)
				default:
					rec[i] = value.Format(x)
				}
			}
			cw.Write(rec)
		}
		cw.Flush()
		return cw.Error()
	case "json":
		var sb strings.Builder
		sb.WriteString("[")
		for r, row := range res.Rows {
			if r > 0 {
				sb.WriteString(",\n ")
			}
			sb.WriteString("{")
			for i, v := range row {
				if i > 0 {
					sb.WriteString(",")
				}
				name, _ := json.Marshal(res.Columns[i])
				sb.Write(name)
				sb.WriteString(":")
				sb.WriteString(jsonValue(v))
			}
			sb.WriteString("}")
		}
		sb.WriteString("]\n")
		_, err := io.WriteString(w, sb.String())
		return err
	case "line":
		width := 0
		for _, c := range res.Columns {
			width = max(width, displayWidth(c))
		}
		for r, row := range res.Rows {
			if r > 0 {
				fmt.Fprintln(w)
			}
			for i, v := range row {
				fmt.Fprintf(w, "%s%s = %s\n", strings.Repeat(" ", width-displayWidth(res.Columns[i])), res.Columns[i], value.Format(v))
			}
		}
	default:
		printTable(w, res)
	}
	return nil
}

// jsonValue renders an SQL value as JSON: numbers as numbers (a DECIMAL
// keeps its digits), a BLOB as a hex string.
func jsonValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case int64, value.Decimal:
		return value.Format(x)
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return "null"
		}
	case []byte:
		v = hex.EncodeToString(x)
	}
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(value.Format(v))
	}
	return string(b)
}

func printTable(w io.Writer, res engine.Result) {
	widths := make([]int, len(res.Columns))
	cells := make([][]string, len(res.Rows))
	for i, c := range res.Columns {
		widths[i] = displayWidth(c)
	}
	for r, row := range res.Rows {
		cells[r] = make([]string, len(row))
		for i, v := range row {
			cells[r][i] = value.Format(v)
			widths[i] = max(widths[i], displayWidth(cells[r][i]))
		}
	}
	line := func(vals []string) {
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = v + strings.Repeat(" ", widths[i]-displayWidth(v))
		}
		fmt.Fprintln(w, strings.TrimRight(strings.Join(parts, " | "), " "))
	}
	line(res.Columns)
	seps := make([]string, len(widths))
	for i, n := range widths {
		seps[i] = strings.Repeat("-", n)
	}
	fmt.Fprintln(w, strings.Join(seps, "-+-"))
	for _, row := range cells {
		line(row)
	}
	fmt.Fprintf(w, "(%d row%s)\n", len(res.Rows), map[bool]string{true: "", false: "s"}[len(res.Rows) == 1])
}

// ---- completion ----

var dotCommands = []string{".backup", ".check", ".dump", ".exit", ".export", ".headers", ".help", ".import", ".mode",
	".output", ".promote", ".quit", ".read", ".repl", ".schema", ".stats", ".tables", ".timer", ".version"}

var sqlKeywords = strings.Fields(`ADD ALL ALTER ANALYZE AND AS ASC AUTOINCREMENT BEGIN BETWEEN BY CASCADE CASE CAST CHECK CLOSURE
	COLLATE COLUMN COMMIT CONFLICT CONSTRAINT CREATE CROSS CURRENT_DATE CURRENT_TIME CURRENT_TIMESTAMP DEFAULT DELETE DESC
	DISTINCT DO DROP ELSE END EXCEPT EXISTS EXPLAIN FOREIGN FROM GENERATED GROUP HAVING IF IN INDEX INNER INSERT INTERSECT
	INTO IS JOIN KEY LEFT LIKE LIMIT NOT NOTHING NULL OFFSET ON OR ORDER OUTER OVER PARTITION PRAGMA PRIMARY RECURSIVE
	REFERENCES RENAME REPLACE RETURNING ROLLBACK SELECT SET TABLE THEN TRIGGER UNION UNIQUE UPDATE USING VACUUM VALUES
	VIEW WHEN WHERE WITH
	INTEGER TEXT REAL BLOB DECIMAL
	COUNT SUM AVG MIN MAX COALESCE IFNULL LENGTH LOWER UPPER SUBSTR ROUND GROUP_CONCAT STRFTIME DATETIME TIME_BUCKET GRAPH_WALK GRAPH_PATH
	JSON_EXTRACT JSON_EACH JSON_TREE JSON_OBJECT JSON_ARRAY GENERATE_SERIES ROW_NUMBER`)

// schemaNames is what completion knows about the database: the names of
// its tables and views, and the columns of each (by lower-case name).
type schemaNames struct {
	tables  []string
	columns map[string][]string
}

// loadNames reads the names from the database.
func loadNames(exec func(string) (engine.Result, error)) *schemaNames {
	n := &schemaNames{columns: map[string][]string{}}
	res, err := exec("PRAGMA table_list")
	if err != nil {
		return n
	}
	for _, row := range res.Rows {
		name, _ := row[0].(string)
		n.tables = append(n.tables, name)
		info, err := exec("PRAGMA table_info('" + strings.ReplaceAll(name, "'", "''") + "')")
		if err != nil {
			continue
		}
		for _, c := range info.Rows {
			if col, ok := c[1].(string); ok {
				n.columns[strings.ToLower(name)] = append(n.columns[strings.ToLower(name)], col)
			}
		}
	}
	return n
}

// completions returns the candidates for the word ending at pos of line
// and where that word starts: dot commands at the start of a line, the
// columns of t after "t.", otherwise tables, columns and SQL keywords
// (spelled in the case of what was typed).
func completions(line []rune, pos int, names *schemaNames) (start int, cands []string) {
	start = pos
	for start > 0 && (isWordRune(line[start-1]) || line[start-1] == '.' && strings.TrimSpace(string(line[:start-1])) == "") {
		start--
	}
	word := string(line[start:pos])
	if word == "" {
		return start, nil
	}
	low := strings.ToLower(word)
	seen := map[string]bool{}
	add := func(list []string) {
		for _, s := range list {
			if strings.HasPrefix(strings.ToLower(s), low) && !seen[strings.ToLower(s)] {
				seen[strings.ToLower(s)] = true
				cands = append(cands, s)
			}
		}
	}
	if strings.HasPrefix(word, ".") {
		add(dotCommands)
		return start, cands
	}
	if start > 0 && line[start-1] == '.' { // the columns of the table before the dot
		q := start - 1
		for q > 0 && isWordRune(line[q-1]) {
			q--
		}
		if cols, ok := names.columns[strings.ToLower(string(line[q:start-1]))]; ok {
			add(cols)
			sort.Strings(cands)
			return start, cands
		}
	}
	add(names.tables)
	var cols []string
	for _, c := range names.columns {
		cols = append(cols, c...)
	}
	sort.Strings(cols)
	add(cols)
	sort.Strings(cands)
	keywords := sqlKeywords
	if word != strings.ToUpper(word) {
		keywords = strings.Fields(strings.ToLower(strings.Join(sqlKeywords, " ")))
	}
	n := len(cands)
	add(keywords)
	sort.Strings(cands[n:])
	return start, cands
}
