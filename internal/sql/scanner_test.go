package sql

import (
	"io"
	"strings"
	"testing"
)

func TestScanner(t *testing.T) {
	src := `-- header comment
PRAGMA foreign_keys=OFF;;
INSERT INTO "a;b" VALUES('x;''y', [c;d], ` + "`e;f`" + `); /* c; */
CREATE TRIGGER tr AFTER INSERT ON t BEGIN
  UPDATE t SET n = n + 1; -- inner;
  DELETE FROM u;
END;
SELECT 1 -- trailing; comment
;
SELECT 2`
	s := NewScanner(strings.NewReader(src))
	var got []string
	for {
		st, err := s.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, st)
	}
	want := []string{
		"-- header comment\nPRAGMA foreign_keys=OFF",
		`INSERT INTO "a;b" VALUES('x;''y', [c;d], ` + "`e;f`" + `)`,
		"/* c; */\nCREATE TRIGGER tr AFTER INSERT ON t BEGIN\n  UPDATE t SET n = n + 1; -- inner;\n  DELETE FROM u;\nEND",
		"SELECT 1 -- trailing; comment",
		"SELECT 2",
	}
	if strings.Join(got, "\n|\n") != strings.Join(want, "\n|\n") {
		t.Errorf("got:\n%s\nwant:\n%s", strings.Join(got, "\n|\n"), strings.Join(want, "\n|\n"))
	}
}
