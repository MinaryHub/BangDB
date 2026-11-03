package httpapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/knoxxr/bangdb/auth"
	"github.com/knoxxr/bangdb/httpapi"
)

// The /v1 API as BangDB Manager ("BangDB (Go)" connections) calls it:
// HTTP Basic with any user and the password, JSON bodies, its response
// field names.
func TestV1ForManager(t *testing.T) {
	_, srv := setup(t, httpapi.Options{Token: "s3cret"})
	do := func(method, path, user, pass, body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Accept", "application/json")
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if user != "" || pass != "" {
			req.SetBasicAuth(user, pass)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		var out map[string]any
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("%s %s: not JSON: %s", method, path, data)
		}
		return resp.StatusCode, out
	}

	// The connection test: health needs no credentials.
	if st, out := do("GET", "/v1/health", "", "", ""); st != 200 || out["ok"] != true || out["version"] == "" {
		t.Fatalf("health: %d %v", st, out)
	}
	if st, _ := do("GET", "/v1/tables", "", "", ""); st != 401 {
		t.Fatalf("tables without credentials: %d", st)
	}
	if st, _ := do("GET", "/v1/tables", "admin", "wrong", ""); st != 401 {
		t.Fatalf("wrong password: %d", st)
	}

	q := func(body string) map[string]any {
		t.Helper()
		st, out := do("POST", "/v1/query", "anyone", "s3cret", body)
		if st != 200 {
			t.Fatalf("query %s: %d %v", body, st, out)
		}
		return out
	}
	// A script in one transaction, as execStatements sends it.
	out := q(`{"sql":"BEGIN;\nCREATE TABLE item (id INTEGER PRIMARY KEY, name TEXT NOT NULL, price DECIMAL(8,2), pic BLOB);\nINSERT INTO item (name, price, pic) VALUES (?1, ?2, ?3);\nCOMMIT;","args":["pen", 1.5, {"blob":"AQID"}],"mode":"exec"}`)
	if out["rows_affected"] == nil || out["last_insert_id"] == nil {
		t.Fatalf("exec fields: %v", out)
	}
	out = q(`{"sql":"INSERT INTO item (name) VALUES (?)","args":["cup"],"mode":"exec"}`)
	if out["last_insert_id"].(float64) != 2 || out["rows_affected"].(float64) != 1 {
		t.Fatalf("insert: %v", out)
	}
	// Reads: auto mode, limit and truncation, blobs and decimals.
	out = q(`{"sql":"SELECT id, name, price, pic FROM item ORDER BY id","limit":1}`)
	if js(out["columns"]) != `["id","name","price","pic"]` || out["truncated"] != true ||
		js(out["rows"]) != `[[1,"pen",1.5,{"blob":"AQID"}]]` || len(out["types"].([]any)) != 4 {
		t.Fatalf("select: %v", out)
	}
	if _, ok := out["ms"].(float64); !ok {
		t.Fatalf("ms: %v", out)
	}
	out = q(`{"sql":"SELECT name FROM item WHERE id = :id","args":{"id":2}}`)
	if js(out["rows"]) != `[["cup"]]` || out["truncated"] != false {
		t.Fatalf("named args: %v", out)
	}

	// Schema for the table pages.
	st, out := do("GET", "/v1/tables", "u", "s3cret", "")
	if st != 200 || out["count"].(float64) != 1 {
		t.Fatalf("tables: %d %v", st, out)
	}
	tbl := out["tables"].([]any)[0].(map[string]any)
	cols := tbl["columns"].([]any)
	if tbl["name"] != "item" || tbl["kind"] != "table" || len(cols) != 4 || !strings.HasPrefix(tbl["sql"].(string), "CREATE TABLE") {
		t.Fatalf("table info: %v", tbl)
	}
	if c := cols[0].(map[string]any); c["name"] != "id" || c["pk"] != true || c["decl"] != "INTEGER" {
		t.Fatalf("column: %v", c)
	}
	q(`{"sql":"CREATE VIEW cheap AS SELECT name FROM item WHERE price < 2","mode":"exec"}`)
	if st, out := do("GET", "/v1/tables/cheap", "u", "s3cret", ""); st != 200 || out["kind"] != "view" {
		t.Fatalf("view: %d %v", st, out)
	}
	if st, out := do("GET", "/v1/tables/nope", "u", "s3cret", ""); st != 404 || out["error"] == nil {
		t.Fatalf("missing table: %d %v", st, out)
	}
	if st, out := do("GET", "/v1/stats", "u", "s3cret", ""); st != 200 || out["tables"].(float64) != 1 || out["views"].(float64) != 1 {
		t.Fatalf("stats: %d %v", st, out)
	}

	// Errors carry "error" and a status the Manager can show.
	if st, out := do("POST", "/v1/query", "u", "s3cret", `{"sql":"INSERT INTO item (id, name) VALUES (1, 'dup')","mode":"exec"}`); st != 409 || out["error"] == nil {
		t.Fatalf("constraint: %d %v", st, out)
	}
	if st, _ := do("POST", "/v1/query", "u", "s3cret", `{"sql":"SELEC 1"}`); st != 400 {
		t.Fatalf("syntax error: %d", st)
	}
	if st, _ := do("POST", "/v1/query", "u", "s3cret", `{"sql":"SELECT 1","mode":"bogus"}`); st != 400 {
		t.Fatalf("bad mode: %d", st)
	}
}

// Roles apply to /v1/query like to /exec.
func TestV1Roles(t *testing.T) {
	users := auth.NewUsers()
	h, _ := auth.HashPassword("reader-pass")
	users.Add("reader", auth.RoleRead, h)
	db, srv := setup(t, httpapi.Options{Users: users})
	if _, err := db.Exec("CREATE TABLE t (v INT)"); err != nil {
		t.Fatal(err)
	}
	post := func(body string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/query", strings.NewReader(body))
		req.SetBasicAuth("reader", "reader-pass")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if st := post(`{"sql":"SELECT count(*) FROM t"}`); st != 200 {
		t.Fatalf("read: %d", st)
	}
	if st := post(`{"sql":"INSERT INTO t VALUES (1)","mode":"exec"}`); st != 403 {
		t.Fatalf("write as reader: %d", st)
	}
}
