package httpapi

// The /v1 API: the endpoints BangDB Manager's "BangDB (Go)" connection type
// uses (github.com/knoxxr/bangdb.manager, src/api/goapi.ts).
//
//	GET  /v1/health          {"ok":true,"version":"..."}           (no authentication)
//	GET  /v1/tables          {"tables":[{name, kind, columns, indexes, primaryKey, store, sql}],"count":N}
//	GET  /v1/tables/{name}   one table or view
//	POST /v1/query           {"sql", "args":[...]|{...}, "mode":"auto|exec|query", "limit":N}
//	                         -> {columns, types, rows, rows_affected, last_insert_id, truncated, ms}
//	GET  /v1/stats           {"version","seq","tables","views","db":{...},"api":{...}}
//
// Credentials, roles, CORS and limits are those of the other endpoints; in
// addition HTTP Basic with any user name and the token as password is
// accepted, as the Manager sends it. Blobs are {"blob": base64} both ways.
// A request runs on its own session: BEGIN … COMMIT inside one script works,
// a transaction cannot span requests.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/knoxxr/bangdb"
	"github.com/knoxxr/bangdb/internal/engine"
	"github.com/knoxxr/bangdb/internal/value"
)

// v1MaxRows caps the rows of one /v1/query answer; a request may ask for
// fewer with "limit".
const v1MaxRows = 1000

func (h *Handler) registerV1() {
	h.mux.HandleFunc("/v1/health", h.get(func(w http.ResponseWriter, r *http.Request) error {
		return writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": bangdb.VersionString()})
	}))
	h.mux.HandleFunc("/v1/tables", h.get(h.v1Tables))
	h.mux.HandleFunc("/v1/tables/", h.get(h.v1Table))
	h.mux.HandleFunc("/v1/query", h.post(h.v1Query))
	h.mux.HandleFunc("/v1/stats", h.get(func(w http.ResponseWriter, r *http.Request) error {
		cat := h.db.Catalog()
		return writeJSON(w, http.StatusOK, map[string]any{
			"version": bangdb.VersionString(),
			"seq":     h.db.Seq(),
			"tables":  len(cat.TableNames()),
			"views":   len(cat.ViewNames()),
			"db":      h.db.Stats(),
			"api":     h.Stats(),
		})
	}))
}

type v1TableInfo struct {
	Name       string          `json:"name"`
	Kind       string          `json:"kind"` // table or view
	Columns    []engine.Column `json:"columns,omitempty"`
	Indexes    []*engine.Index `json:"indexes,omitempty"`
	PrimaryKey []string        `json:"primaryKey,omitempty"`
	Store      string          `json:"store,omitempty"`
	SQL        string          `json:"sql"`
}

func (h *Handler) v1Describe(cat *engine.Catalog, name string) *v1TableInfo {
	if t := cat.Table(name); t != nil {
		return &v1TableInfo{Name: t.Name, Kind: "table", Columns: t.Columns, Indexes: t.Indexes(),
			PrimaryKey: t.PrimaryKey, Store: t.Store, SQL: cat.Describe(t.Name)}
	}
	for _, v := range cat.ViewNames() {
		if strings.EqualFold(v, name) {
			return &v1TableInfo{Name: v, Kind: "view", SQL: cat.DescribeView(v)}
		}
	}
	return nil
}

func (h *Handler) v1Tables(w http.ResponseWriter, r *http.Request) error {
	cat := h.db.Catalog()
	names := append(cat.TableNames(), cat.ViewNames()...)
	sort.Strings(names)
	list := make([]*v1TableInfo, 0, len(names))
	for _, n := range names {
		if t := h.v1Describe(cat, n); t != nil {
			list = append(list, t)
		}
	}
	return writeJSON(w, http.StatusOK, map[string]any{"tables": list, "count": len(list)})
}

func (h *Handler) v1Table(w http.ResponseWriter, r *http.Request) error {
	name := strings.TrimPrefix(r.URL.Path, "/v1/tables/")
	t := h.v1Describe(h.db.Catalog(), name)
	if t == nil {
		return &apiError{http.StatusNotFound, "not_found", fmt.Sprintf("bangdb: no table or view %q", name)}
	}
	return writeJSON(w, http.StatusOK, t)
}

type v1QueryRequest struct {
	SQL   string          `json:"sql"`
	Args  json.RawMessage `json:"args"`
	Mode  string          `json:"mode"` // auto (default), exec, query
	Limit int             `json:"limit"`
}

type v1QueryResponse struct {
	Columns      []string `json:"columns"`
	Types        []string `json:"types"`
	Rows         [][]any  `json:"rows"`
	RowsAffected int64    `json:"rows_affected"`
	LastInsertID int64    `json:"last_insert_id"`
	Truncated    bool     `json:"truncated"`
	Millis       float64  `json:"ms"`
}

func (h *Handler) v1Query(w http.ResponseWriter, r *http.Request) error {
	var req v1QueryRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	if strings.TrimSpace(req.SQL) == "" {
		return badRequest(`bangdb: "sql" is required`)
	}
	if err := h.permit(r, req.SQL); err != nil {
		return err
	}
	ro, err := h.db.ReadOnlySQL(req.SQL)
	if err != nil {
		return err
	}
	if h.opts.ReadOnly && !ro {
		return errReadOnlyAPI
	}
	args, err := v1Args(req.Args)
	if err != nil {
		return err
	}
	limit := v1MaxRows
	if req.Limit > 0 && req.Limit < limit {
		limit = req.Limit
	}
	mode := strings.ToLower(req.Mode)
	if mode == "" || mode == "auto" {
		mode = "exec"
		if ro && !isScript(req.SQL) {
			mode = "query"
		}
	}
	conn := h.db.Conn()
	defer conn.Close()
	start := time.Now()
	resp := v1QueryResponse{Columns: []string{}, Types: []string{}, Rows: [][]any{}}
	switch mode {
	case "exec":
		res, err := conn.ExecContext(r.Context(), req.SQL, args...)
		if err != nil {
			return err
		}
		resp.RowsAffected, resp.LastInsertID = res.RowsAffected, res.LastInsertID
		if len(res.Columns) > 0 {
			resp.Columns, resp.Types = res.Columns, padTypes(res.Types, len(res.Columns))
			for i, row := range res.Rows {
				if i >= limit {
					resp.Truncated = true
					break
				}
				resp.Rows = append(resp.Rows, v1Row(row))
			}
		}
	case "query":
		rows, err := conn.QueryRows(r.Context(), req.SQL, args...)
		if err != nil {
			return err
		}
		resp.Columns, resp.Types = rows.Columns(), padTypes(rows.ColumnTypes(), len(rows.Columns()))
		for rows.Next() {
			if len(resp.Rows) >= limit {
				resp.Truncated = true
				break
			}
			resp.Rows = append(resp.Rows, v1Row(rows.Row()))
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	default:
		return badRequest(fmt.Sprintf("bangdb: unknown mode %q (auto, exec or query)", req.Mode))
	}
	resp.Millis = float64(time.Since(start).Microseconds()) / 1000
	return writeJSON(w, http.StatusOK, resp)
}

// isScript reports whether src holds more than one statement.
func isScript(src string) bool {
	return strings.Contains(strings.TrimRight(strings.TrimSpace(src), "; \t\r\n"), ";")
}

func padTypes(types []string, n int) []string {
	out := make([]string, n)
	copy(out, types)
	return out
}

func v1Row(row []any) []any {
	out := make([]any, len(row))
	for i, v := range row {
		switch x := v.(type) {
		case []byte:
			out[i] = map[string]string{"blob": base64.StdEncoding.EncodeToString(x)}
		case value.Decimal:
			out[i] = json.Number(x.String())
		case float64:
			if math.IsNaN(x) || math.IsInf(x, 0) {
				out[i] = nil
			} else {
				out[i] = x
			}
		default:
			out[i] = v
		}
	}
	return out
}

// v1Args decodes positional (array) or named (object) arguments, where a
// blob is {"blob": base64} (the other endpoints use {"$blob": …}).
func v1Args(raw json.RawMessage) ([]any, error) {
	if t := strings.TrimSpace(string(raw)); t == "" || t == "null" {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, badRequest("bangdb: invalid args: " + err.Error())
	}
	conv := func(x any) (any, error) {
		if m, ok := x.(map[string]any); ok && len(m) == 1 {
			if s, ok := m["blob"].(string); ok {
				b, err := base64.StdEncoding.DecodeString(s)
				if err != nil {
					return nil, badRequest("bangdb: invalid blob: " + err.Error())
				}
				return b, nil
			}
		}
		return fromJSON(x)
	}
	switch a := v.(type) {
	case []any:
		out := make([]any, len(a))
		for i, x := range a {
			c, err := conv(x)
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil
	case map[string]any:
		out := make([]any, 0, len(a))
		for name, x := range a {
			c, err := conv(x)
			if err != nil {
				return nil, err
			}
			out = append(out, bangdb.Named(name, c))
		}
		return out, nil
	}
	return nil, badRequest("bangdb: args must be a list or an object")
}
