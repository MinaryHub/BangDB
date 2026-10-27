package httpapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/knoxxr/bangdb"
	"github.com/knoxxr/bangdb/httpapi"
)

func setup(t *testing.T, opts httpapi.Options) (*bangdb.DB, *httptest.Server) {
	t.Helper()
	db, err := bangdb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(httpapi.New(db, opts))
	t.Cleanup(func() { srv.Close(); db.Close() })
	return db, srv
}

func call(t *testing.T, srv *httptest.Server, method, path, token, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("%s %s: invalid JSON %q", method, path, data)
	}
	return resp.StatusCode, out
}

func js(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestHTTPAPI(t *testing.T) {
	_, srv := setup(t, httpapi.Options{Token: "tok"})
	if code, out := call(t, srv, "GET", "/health", "", ""); code != 401 || out["code"] != "unauthorized" {
		t.Fatalf("no token: %d %v", code, out)
	}
	if code, out := call(t, srv, "GET", "/health", "tok", ""); code != 200 || out["status"] != "ok" {
		t.Fatalf("health: %d %v", code, out)
	}
	code, out := call(t, srv, "POST", "/exec", "tok", `{"sql": "CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT UNIQUE, score REAL, data BLOB, big INT)"}`)
	if code != 200 {
		t.Fatalf("create: %d %v", code, out)
	}
	code, out = call(t, srv, "POST", "/exec", "tok", `{"sql": "INSERT INTO t (name, score, data, big) VALUES (?, ?, ?, ?) RETURNING id, data",
		"args": ["kim", 1.5, {"$blob": "AAH/"}, 9007199254740993]}`)
	if code != 200 || out["rowsAffected"] != 1.0 || out["lastInsertId"] != 1.0 || js(out["rows"]) != `[[1,{"$blob":"AAH/"}]]` {
		t.Fatalf("insert: %d %v", code, out)
	}
	code, out = call(t, srv, "POST", "/exec", "tok", `{"sql": "INSERT INTO t (name, score) VALUES (:n, :s)", "args": {"n": "lee", "s": true}}`)
	if code != 200 {
		t.Fatalf("named insert: %d %v", code, out)
	}
	// Large integers stay exact in the raw response.
	req, _ := http.NewRequest("POST", srv.URL+"/query", strings.NewReader(`{"sql": "SELECT id, name, score, data, big FROM t ORDER BY id"}`))
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var q struct {
		Columns []string
		Types   []string
		Rows    [][]json.RawMessage
	}
	if err := json.Unmarshal(raw, &q); err != nil {
		t.Fatalf("query JSON: %v %s", err, raw)
	}
	if strings.Join(q.Columns, ",") != "id,name,score,data,big" || q.Types[1] != "TEXT" || len(q.Rows) != 2 ||
		string(q.Rows[0][4]) != "9007199254740993" || string(q.Rows[1][2]) != "1" || string(q.Rows[1][3]) != "null" {
		t.Fatalf("query: %s", raw)
	}

	// Errors.
	for _, c := range []struct {
		method, path, body string
		code               int
		kind               string
	}{
		{"POST", "/exec", `{"sql": "INSERT INTO t (name) VALUES ('kim')"}`, 409, "constraint"},
		{"POST", "/query", `{"sql": "SELECT * FROM nope"}`, 400, "sql"},
		{"POST", "/query", `{"sql": "DELETE FROM t"}`, 400, "request"},
		{"POST", "/exec", `{"sql": "BEGIN"}`, 400, "sql"},
		{"POST", "/exec", `{bad json`, 400, "request"},
		{"POST", "/exec", `{"sql": "SELECT ?", "args": [[1]]}`, 400, "request"},
		{"GET", "/exec", ``, 405, "method"},
	} {
		code, out := call(t, srv, c.method, c.path, "tok", c.body)
		if code != c.code || out["code"] != c.kind {
			t.Errorf("%s %s %s: %d %v, want %d %s", c.method, c.path, c.body, code, out, c.code, c.kind)
		}
	}

	// Batch: one transaction, all or nothing.
	code, out = call(t, srv, "POST", "/batch", "tok", `{"statements": [
		{"sql": "INSERT INTO t (name) VALUES ('a')"},
		{"sql": "INSERT INTO t (name) VALUES ('kim')"}]}`)
	if code != 409 || out["statement"] != 1.0 {
		t.Fatalf("failing batch: %d %v", code, out)
	}
	code, out = call(t, srv, "POST", "/batch", "tok", `{"statements": [
		{"sql": "INSERT INTO t (name) VALUES (?)", "args": ["b"]},
		{"sql": "UPDATE t SET score = 9 WHERE name IN ('b', 'kim')"},
		{"sql": "SELECT count(*) FROM t"}]}`)
	if code != 200 || js(out["results"]) != `[{"lastInsertId":3,"rowsAffected":1},{"lastInsertId":0,"rowsAffected":2},{"columns":["COUNT(*)"],"lastInsertId":0,"rows":[[3]],"rowsAffected":0}]` {
		t.Fatalf("batch: %d %s", code, js(out))
	}
	if code, out := call(t, srv, "GET", "/stats", "tok", ""); code != 200 || out["TxCommits"] == nil {
		t.Fatalf("stats: %d %v", code, out)
	}
}

func TestHTTPAPICORSAndReadOnly(t *testing.T) {
	_, srv := setup(t, httpapi.Options{AllowOrigins: []string{"https://admin.example"}, ReadOnly: true})
	req, _ := http.NewRequest("OPTIONS", srv.URL+"/query", nil)
	req.Header.Set("Origin", "https://admin.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 || resp.Header.Get("Access-Control-Allow-Origin") != "https://admin.example" ||
		!strings.Contains(resp.Header.Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Fatalf("preflight: %d %v", resp.StatusCode, resp.Header)
	}
	req, _ = http.NewRequest("GET", srv.URL+"/health", nil)
	req.Header.Set("Origin", "https://evil.example")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("CORS allowed for an unlisted origin")
	}
	if code, out := call(t, srv, "POST", "/exec", "", `{"sql": "CREATE TABLE x (a)"}`); code != 403 || out["code"] != "readonly" {
		t.Fatalf("read-only exec: %d %v", code, out)
	}
	if code, out := call(t, srv, "POST", "/query", "", `{"sql": "SELECT 1 + ?", "args": [2]}`); code != 200 || js(out["rows"]) != "[[3]]" {
		t.Fatalf("read-only query: %d %v", code, out)
	}
}
