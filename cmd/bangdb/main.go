// Command bangdb is an interactive SQL shell for BangDB databases.
//
//	bangdb app.db                 # interactive shell
//	bangdb app.db "SELECT 1"      # run SQL and exit
//	echo "SELECT 1;" | bangdb app.db
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/knoxxr/bangdb/internal/engine"
	"github.com/knoxxr/bangdb/internal/value"
)

func main() {
	path := ":memory:"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	db, err := engine.Open(path, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer db.Close()
	sh := &shell{db: db, conn: db.Conn(), out: os.Stdout}
	if len(os.Args) > 2 {
		ok := sh.run(strings.Join(os.Args[2:], " "))
		sh.conn.Close()
		if !ok {
			os.Exit(1)
		}
		return
	}
	interactive := isTerminal()
	if interactive {
		fmt.Printf("BangDB shell — %s\nEnter \".help\" for usage hints.\n", path)
	}
	sh.loop(os.Stdin, interactive)
	sh.conn.Close()
}

func isTerminal() bool {
	st, err := os.Stdin.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

type shell struct {
	db    *engine.DB
	conn  *engine.Conn
	out   io.Writer
	timer bool
}

func (s *shell) loop(in io.Reader, interactive bool) {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	var buf strings.Builder
	prompt := func() {
		if !interactive {
			return
		}
		switch {
		case buf.Len() > 0:
			fmt.Print("   ...> ")
		case s.conn.InTx():
			fmt.Print("bangdb*> ")
		default:
			fmt.Print("bangdb> ")
		}
	}
	prompt()
	for sc.Scan() {
		line := sc.Text()
		if buf.Len() == 0 && strings.HasPrefix(strings.TrimSpace(line), ".") {
			if !s.command(strings.TrimSpace(line)) {
				return
			}
			prompt()
			continue
		}
		buf.WriteString(line)
		buf.WriteString("\n")
		if strings.HasSuffix(strings.TrimSpace(line), ";") {
			s.run(buf.String())
			buf.Reset()
		}
		prompt()
	}
	if strings.TrimSpace(buf.String()) != "" {
		s.run(buf.String())
	}
}

func (s *shell) command(cmd string) bool {
	fields := strings.Fields(cmd)
	switch fields[0] {
	case ".quit", ".exit":
		return false
	case ".help":
		fmt.Fprintln(s.out, `.tables            list tables
.schema [table]    show table definitions
.timer on|off      show execution time
.quit              exit
SQL statements end with ';'. BEGIN/COMMIT/ROLLBACK are supported.`)
	case ".tables":
		names := s.db.Catalog().TableNames()
		sort.Strings(names)
		fmt.Fprintln(s.out, strings.Join(names, "  "))
	case ".schema":
		names := s.db.Catalog().TableNames()
		sort.Strings(names)
		for _, n := range names {
			if len(fields) > 1 && !strings.EqualFold(fields[1], n) {
				continue
			}
			fmt.Fprintln(s.out, s.db.Catalog().Describe(n))
		}
	case ".timer":
		s.timer = len(fields) > 1 && fields[1] == "on"
	default:
		fmt.Fprintf(s.out, "unknown command %s (try .help)\n", fields[0])
	}
	return true
}

func (s *shell) run(src string) bool {
	start := time.Now()
	res, err := s.conn.Exec(src)
	if err != nil {
		fmt.Fprintln(s.out, "Error:", err)
		return false
	}
	if len(res.Columns) > 0 {
		printTable(s.out, res)
	} else if res.RowsAffected > 0 {
		fmt.Fprintf(s.out, "%d row(s) affected\n", res.RowsAffected)
	}
	if s.timer {
		fmt.Fprintf(s.out, "Run Time: %s\n", time.Since(start))
	}
	return true
}

func printTable(w io.Writer, res engine.Result) {
	widths := make([]int, len(res.Columns))
	cells := make([][]string, len(res.Rows))
	for i, c := range res.Columns {
		widths[i] = utf8.RuneCountInString(c)
	}
	for r, row := range res.Rows {
		cells[r] = make([]string, len(row))
		for i, v := range row {
			cells[r][i] = value.Format(v)
			widths[i] = max(widths[i], utf8.RuneCountInString(cells[r][i]))
		}
	}
	line := func(vals []string) {
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = v + strings.Repeat(" ", widths[i]-utf8.RuneCountInString(v))
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
