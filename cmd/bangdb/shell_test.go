package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knoxxr/bangdb"
	"github.com/knoxxr/bangdb/internal/engine"
)

// testEditor returns an editor fed with keys, on a terminal 40 columns
// wide; what it draws is discarded.
func testEditor(keys string) *lineEditor {
	ed := newLineEditor(strings.NewReader(keys), io.Discard, "")
	ed.raw = func() (func(), bool) { return func() {}, true }
	ed.width = func() int { return 40 }
	return ed
}

func TestLineEditorKeys(t *testing.T) {
	const (
		left, right, up, down = "\x1b[D", "\x1b[C", "\x1b[A", "\x1b[B"
		home, end, del        = "\x1b[H", "\x1b[F", "\x1b[3~"
		wordLeft, wordRight   = "\x1b[1;5D", "\x1b[1;5C"
	)
	for _, c := range []struct{ keys, want string }{
		{"select 1;\r", "select 1;"},
		{"select 1;\n", "select 1;"},
		{"selct" + left + left + "e\r", "select"},
		{"abc" + home + "x" + end + "y\r", "xabcy"},
		{"abc\x01x\x05y\r", "xabcy"},                          // Ctrl-A, Ctrl-E
		{"abc\x7f\x7fx\r", "ax"},                              // Backspace
		{"abc" + home + del + del + "\r", "c"},                // Delete
		{"abc" + home + "\x04\r", "bc"},                       // Ctrl-D deletes when the line is not empty
		{"one two three\x17\x17x\r", "one x"},                 // Ctrl-W
		{"one two" + left + left + left + "\x0bX\r", "one X"}, // Ctrl-K
		{"one two" + left + left + left + "\x15\r", "two"},    // Ctrl-U
		{"aa bb cc" + wordLeft + wordLeft + "X" + wordRight + "Y\r", "aa XbbY cc"},
		{"aa bb\x1bb" + "X\x1bfY\r", "aa XbbY"},        // Alt-B, Alt-F
		{"한글 테스트\x7f" + left + left + "!\r", "한글 !테스"}, // wide characters
		{"a\tb\r", "a b"},                              // a pasted tab is white space
		{"ab\x1bOH" + "x\x1bOF" + "y\r", "xaby"},       // Home and End as SS3 sequences
		{strings.Repeat("0123456789", 12) + home + "<" + end + ">\r", "<" + strings.Repeat("0123456789", 12) + ">"}, // wider than the terminal
		{"last line", "last line"}, // input ends without a newline
	} {
		got, err := testEditor(c.keys).readLine("> ")
		if err != nil || got != c.want {
			t.Errorf("keys %q: got %q, %v; want %q", c.keys, got, err, c.want)
		}
	}

	// Several lines; CR LF counts once; Ctrl-C abandons; Ctrl-D ends.
	ed := testEditor("first\r\nsecond\rthird\x03fourth\r\x04")
	var got []string
	for {
		line, err := ed.readLine("> ")
		if err == errAbandoned {
			got = append(got, "^C")
			continue
		}
		if err != nil {
			if err != io.EOF {
				t.Fatal(err)
			}
			break
		}
		got = append(got, line)
	}
	if s := strings.Join(got, "|"); s != "first|second|^C|fourth" {
		t.Errorf("lines: %s", s)
	}

	// History: up and down, the unfinished line kept, edits of an entry.
	ed = testEditor("one\rtwo\r" + up + "\r" + "new" + up + up + down + down + "!\r" + up + up + up + up + up + "\x7fE\r")
	got = nil
	for {
		line, err := ed.readLine("> ")
		if err != nil {
			break
		}
		got = append(got, line)
	}
	if s := strings.Join(got, "|"); s != "one|two|two|new!|onE" {
		t.Errorf("history: %s", s)
	}
	if h := strings.Join(ed.history, "|"); h != "one|two|new!|onE" { // no repeats in a row
		t.Errorf("history kept: %s", h)
	}
}

func TestLineEditorDrawing(t *testing.T) {
	var out bytes.Buffer
	ed := testEditor("한a" + "\x1b[D" + "\r")
	ed.out = &out
	if _, err := ed.readLine("db> "); err != nil {
		t.Fatal(err)
	}
	// After the last key before Enter: the line, then the cursor on "a",
	// 4 (prompt) + 2 (the wide character) columns from the left.
	if !strings.Contains(out.String(), "\rdb> 한a\x1b[K\r\x1b[6C") {
		t.Errorf("drawn: %q", out.String())
	}
	// A long line shows the part around the cursor.
	out.Reset()
	ed = testEditor(strings.Repeat("x", 100) + "END\r")
	ed.out = &out
	ed.readLine("> ")
	last := out.String()[strings.LastIndex(out.String(), "\r> ")+3:]
	if shown, _, _ := strings.Cut(last, "\x1b[K"); !strings.HasSuffix(shown, "xEND") || len(shown) != 37 { // 40 - prompt - 1
		t.Errorf("long line shows %q", shown)
	}
}

func TestLineEditorHistoryFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hist")
	ed := newLineEditor(strings.NewReader("select 1;\r  \rselect 2;\r"), io.Discard, path)
	ed.raw = func() (func(), bool) { return func() {}, true }
	for {
		if _, err := ed.readLine(""); err != nil {
			break
		}
	}
	data, _ := os.ReadFile(path)
	if string(data) != "select 1;\nselect 2;\n" {
		t.Errorf("history file: %q", data)
	}
	// The next session recalls it.
	ed = newLineEditor(strings.NewReader("\x1b[A\x1b[A\r"), io.Discard, path)
	ed.raw = func() (func(), bool) { return func() {}, true }
	if line, _ := ed.readLine(""); line != "select 1;" {
		t.Errorf("recalled %q", line)
	}
	if historyFile("off") != "" || historyFile("/x/h") != "/x/h" || !strings.HasSuffix(historyFile(""), ".bangdb_history") {
		t.Error("historyFile")
	}
}

func testShell(t *testing.T) (*shell, *bytes.Buffer) {
	db, err := bangdb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var out bytes.Buffer
	return newShell(db.Engine(), localSession{db.Engine().Conn()}, &out), &out
}

func TestCompletion(t *testing.T) {
	sh, _ := testShell(t)
	sh.run(`CREATE TABLE users (id INTEGER PRIMARY KEY, user_name TEXT, email TEXT);
		CREATE TABLE user_logs (id INTEGER PRIMARY KEY, uid INT, note TEXT);
		CREATE VIEW emails AS SELECT email FROM users;`)
	names := loadNames(sh.conn.Exec)
	for _, c := range []struct {
		line string
		want string // candidates, or the word's start position with "@"
	}{
		{"select * from us", "user_logs user_name users using"},
		{"SELECT * FROM US", "user_logs user_name users USING"},
		{"select em", "email emails"},
		{"select users.", ""},
		{"select users.e", "email"},
		{"select user_logs.", ""},
		{"select user_logs.n", "note"},
		{"sel", "select"},
		{"SEL", "SELECT"},
		{".he", ".headers .help"},
		{"  .mo", ".mode"},
		{"select .he", ""},
		{"select zzz", ""},
		{"select ", ""},
	} {
		line := []rune(c.line)
		_, cands := completions(line, len(line), names)
		if got := strings.Join(cands, " "); got != c.want {
			t.Errorf("%q: got %q, want %q", c.line, got, c.want)
		}
	}

	// Through the editor: a single candidate is completed, several extend
	// to what they share, a second Tab lists them.
	var screen bytes.Buffer
	ed := testEditor("")
	ed.out = &screen
	ed.complete = func(line []rune, pos int) (int, []string) { return completions(line, pos, names) }
	type step struct{ keys, want string }
	for _, c := range []step{
		{"sel\t* fr\tuser_l\t\r", "select * from user_logs "},
		{"select em\t\r", "select email"},
		{"select user_\t\r", "select user_"},
		{"select us\t\t\r", "select us"},
		{"SELECT users.e\t\r", "SELECT users.email "},
		{".sc\t\r", ".schema "},
		{"select zz\t\r", "select zz"},
	} {
		screen.Reset()
		ed.in.Reset(&tabReader{keys: c.keys})
		got, err := ed.readLine("> ")
		if err != nil || got != c.want {
			t.Errorf("keys %q: got %q, %v; want %q", c.keys, got, err, c.want)
		}
		if strings.Contains(c.keys, "\t\t") && !strings.Contains(screen.String(), "user_logs  user_name  users") {
			t.Errorf("candidates not listed: %q", screen.String())
		}
	}
}

// tabReader delivers keys one at a time, as typing does (a Tab followed
// at once by more input is taken for pasted text).
type tabReader struct{ keys string }

func (r *tabReader) Read(p []byte) (int, error) {
	if r.keys == "" {
		return 0, io.EOF
	}
	n := 1
	if r.keys[0] == 0x1b { // an escape sequence arrives whole
		for n < len(r.keys) && !(r.keys[n] >= 0x40 && r.keys[n] <= 0x7e && r.keys[n] != '[' && r.keys[n] != 'O') {
			n++
		}
		n = min(n+1, len(r.keys))
	}
	n = copy(p, r.keys[:n])
	r.keys = r.keys[n:]
	return n, nil
}

func TestOutputModes(t *testing.T) {
	res := engine.Result{Columns: []string{"id", "이름", "v"}, Rows: [][]any{
		{int64(1), "김", 1.5}, {int64(2), `a,"b"`, nil}, {int64(3), "x", []byte{0xab, 0x01}}}}
	for mode, want := range map[string]string{
		"table": "id | 이름  | v\n---+-------+--------\n1  | 김    | 1.5\n2  | a,\"b\" | NULL\n3  | x     | x'ab01'\n(3 rows)\n",
		"list":  "id|이름|v\n1|김|1.5\n2|a,\"b\"|NULL\n3|x|x'ab01'\n",
		"csv":   "id,이름,v\n1,김,1.5\n2,\"a,\"\"b\"\"\",\n3,x,ab01\n",
		"json":  "[{\"id\":1,\"이름\":\"김\",\"v\":1.5},\n {\"id\":2,\"이름\":\"a,\\\"b\\\"\",\"v\":null},\n {\"id\":3,\"이름\":\"x\",\"v\":\"ab01\"}]\n",
		"line":  "  id = 1\n이름 = 김\n   v = 1.5\n\n  id = 2\n이름 = a,\"b\"\n   v = NULL\n\n  id = 3\n이름 = x\n   v = x'ab01'\n",
	} {
		var out bytes.Buffer
		if err := printResult(&out, res, mode, true); err != nil || out.String() != want {
			t.Errorf("%s: %v\n got: %q\nwant: %q", mode, err, out.String(), want)
		}
	}
	var out bytes.Buffer
	printResult(&out, res, "csv", false)
	if !strings.HasPrefix(out.String(), "1,김,1.5\n") {
		t.Errorf("csv without headers: %q", out.String())
	}
	out.Reset()
	printResult(&out, engine.Result{Columns: []string{"a"}}, "json", true)
	if out.String() != "[]\n" {
		t.Errorf("empty json: %q", out.String())
	}
}

func TestShellCommands(t *testing.T) {
	sh, out := testShell(t)
	run := func(lines ...string) string {
		t.Helper()
		out.Reset()
		var buf strings.Builder
		for _, l := range lines {
			if !sh.feed(&buf, l) {
				t.Fatalf("%s: the shell ended", l)
			}
		}
		return out.String()
	}
	run("CREATE TABLE t (id INTEGER PRIMARY KEY,", "  name TEXT);", "CREATE INDEX t_name ON t (lower(name));",
		"CREATE VIEW v AS SELECT name FROM t;", "INSERT INTO t (name) VALUES ('a'), ('b');")
	if got := run(".tables"); got != "t  v\n" {
		t.Errorf(".tables: %q", got)
	}
	if got := run(".schema"); !strings.Contains(got, "CREATE TABLE t (") || !strings.Contains(got, "CREATE INDEX t_name ON t (LOWER(name));") ||
		!strings.Contains(got, "CREATE VIEW v AS SELECT name FROM t;") {
		t.Errorf(".schema: %q", got)
	}
	if got := run(".schema v"); got != "CREATE VIEW v AS SELECT name FROM t;\n" {
		t.Errorf(".schema v: %q", got)
	}
	if got := run(".schema nope"); !strings.Contains(got, "no such table") {
		t.Errorf(".schema nope: %q", got)
	}
	if got := run(".mode"); got != "table\n" {
		t.Errorf(".mode: %q", got)
	}
	if got := run(".mode csv", "SELECT * FROM t;"); got != "id,name\n1,a\n2,b\n" {
		t.Errorf("csv: %q", got)
	}
	if got := run(".headers off", ".mode list", "SELECT * FROM t;"); got != "1|a\n2|b\n" {
		t.Errorf("list: %q", got)
	}
	if got := run(".mode nope"); !strings.Contains(got, "unknown mode") {
		t.Errorf(".mode nope: %q", got)
	}
	// .output sends results to a file; messages stay on the screen.
	path := filepath.Join(t.TempDir(), "out.json")
	got := run(".mode json", ".output "+path, "SELECT id FROM t;", "SELECT nope;", ".output", "SELECT 7 AS n;")
	if !strings.Contains(got, "Error:") || !strings.HasSuffix(got, "[{\"n\":7}]\n") || strings.Contains(got, `"id"`) {
		t.Errorf("screen during .output: %q", got)
	}
	if data, _ := os.ReadFile(path); string(data) != "[{\"id\":1},\n {\"id\":2}]\n" {
		t.Errorf("output file: %q", data)
	}

	// Command-line arguments: commands and SQL.
	out.Reset()
	if !sh.runArgs([]string{".mode list", "SELECT", "COUNT(*) FROM t", ".mode line", "SELECT 1 AS one"}) {
		t.Error("runArgs failed")
	}
	if out.String() != "2\none = 1\n" {
		t.Errorf("runArgs: %q", out.String())
	}
	if sh.runArgs([]string{"SELECT nope"}) {
		t.Error("runArgs with an error succeeded")
	}
	if sh.feed(new(strings.Builder), ".quit") {
		t.Error(".quit did not end the shell")
	}
}
