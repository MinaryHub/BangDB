// Package httpapi serves a BangDB database over HTTP with JSON, for
// programs in any language, browsers and web tools. It complements the
// binary protocol of package server, which is faster and keeps sessions.
//
//	h := httpapi.New(db, httpapi.Options{Token: "s3cret", AllowOrigins: []string{"https://admin.example.com"}})
//	http.ListenAndServe("127.0.0.1:18080", h)
//
// Endpoints (request and response bodies are JSON):
//
//	POST /query  {"sql": "...", "args": [...]}   rows of a SELECT / EXPLAIN
//	             -> {"columns": [...], "types": [...], "rows": [[...], ...]}
//	POST /exec   {"sql": "...", "args": [...]}   any statement(s)
//	             -> {"rowsAffected": n, "lastInsertId": n, "columns": [...], "rows": [...]}
//	POST /batch  {"statements": [{"sql": ..., "args": ...}, ...]}
//	             all in one transaction -> {"results": [<exec result>, ...]}
//	GET  /health -> {"status": "ok", "version": "..."}
//	GET  /stats  -> DB.Stats()
//
// "args" is a list for ? placeholders or an object for :name placeholders.
// Values: null, numbers (integers stay exact; integral numbers become
// INTEGER), strings, true/false (1/0), and {"$blob": "<base64>"} for
// BLOBs, which results use too. Errors are {"error": "...", "code": "..."}
// with an HTTP status: 400 SQL error, 401 unauthorized, 403 read-only, 409
// constraint, 499 interrupted, 503 busy.
//
// Each request is independent: there are no sessions, so a transaction is
// one /batch request (BEGIN/COMMIT statements are refused).
package httpapi

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/knoxxr/bangdb"
	"github.com/knoxxr/bangdb/internal/engine"
)

// Options configures the handler.
type Options struct {
	// Token, if set, must be sent as "Authorization: Bearer <token>".
	Token string
	// Authenticate, if set, decides instead of Token.
	Authenticate func(r *http.Request) bool
	// AllowOrigins lists the origins browsers may call from (CORS); "*"
	// allows any. Empty: no CORS headers (same-origin or non-browser use).
	AllowOrigins []string
	// ReadOnly refuses /exec and /batch.
	ReadOnly bool
	// MaxBodyBytes bounds a request body. Default: 16 MiB.
	MaxBodyBytes int64
	// MaxConcurrent bounds the requests run at once; others wait for a
	// slot (or until they are canceled). 0: unlimited.
	MaxConcurrent int
}

// Handler is an http.Handler serving one database.
type Handler struct {
	db    *engine.DB
	opts  Options
	mux   *http.ServeMux
	slots chan struct{} // MaxConcurrent
}

// New returns a handler for db.
func New(db *bangdb.DB, opts Options) *Handler {
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = 16 << 20
	}
	h := &Handler{db: db.Engine(), opts: opts, mux: http.NewServeMux()}
	if opts.MaxConcurrent > 0 {
		h.slots = make(chan struct{}, opts.MaxConcurrent)
	}
	h.mux.HandleFunc("/query", h.post(h.query))
	h.mux.HandleFunc("/exec", h.post(h.exec))
	h.mux.HandleFunc("/batch", h.post(h.batch))
	h.mux.HandleFunc("/health", h.get(func(w http.ResponseWriter, r *http.Request) error {
		return writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": bangdb.Version, "readOnly": h.db.ReadOnly()})
	}))
	h.mux.HandleFunc("/stats", h.get(func(w http.ResponseWriter, r *http.Request) error {
		return writeJSON(w, http.StatusOK, h.db.Stats())
	}))
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if origin := r.Header.Get("Origin"); origin != "" && h.allowed(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
		if r.Method == http.MethodOptions { // preflight
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	if !h.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="bangdb"`)
		writeError(w, http.StatusUnauthorized, "unauthorized", "bangdb: authentication required")
		return
	}
	if h.slots != nil {
		select {
		case h.slots <- struct{}{}:
			defer func() { <-h.slots }()
		case <-r.Context().Done():
			writeError(w, http.StatusServiceUnavailable, "busy", "bangdb: too many concurrent requests")
			return
		}
	}
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) allowed(origin string) bool {
	for _, o := range h.opts.AllowOrigins {
		if o == "*" || strings.EqualFold(o, origin) {
			return true
		}
	}
	return false
}

func (h *Handler) authorized(r *http.Request) bool {
	if h.opts.Authenticate != nil {
		return h.opts.Authenticate(r)
	}
	if h.opts.Token == "" {
		return true
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(tok), []byte(h.opts.Token)) == 1
}

type handlerFunc func(w http.ResponseWriter, r *http.Request) error

func (h *Handler) post(fn handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method", "bangdb: use POST")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, h.opts.MaxBodyBytes)
		if err := fn(w, r); err != nil {
			writeEngineError(w, err)
		}
	}
}

func (h *Handler) get(fn handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method", "bangdb: use GET")
			return
		}
		if err := fn(w, r); err != nil {
			writeEngineError(w, err)
		}
	}
}

// ---- requests ----

type statement struct {
	SQL  string          `json:"sql"`
	Args json.RawMessage `json:"args"`
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return badRequest(fmt.Sprintf("bangdb: invalid JSON body: %v", err))
	}
	return nil
}

func (st statement) args() ([]any, error) {
	if len(bytes.TrimSpace(st.Args)) == 0 || string(bytes.TrimSpace(st.Args)) == "null" {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(st.Args))
	dec.UseNumber()
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return nil, badRequest("bangdb: invalid args: " + err.Error())
	}
	switch a := raw.(type) {
	case []any:
		out := make([]any, len(a))
		for i, v := range a {
			x, err := fromJSON(v)
			if err != nil {
				return nil, err
			}
			out[i] = x
		}
		return out, nil
	case map[string]any:
		var out []any
		for name, v := range a {
			x, err := fromJSON(v)
			if err != nil {
				return nil, err
			}
			out = append(out, bangdb.Named(name, x))
		}
		return out, nil
	}
	return nil, badRequest("bangdb: args must be a list or an object")
}

// fromJSON converts a decoded JSON value to an SQL value.
func fromJSON(v any) (any, error) {
	switch x := v.(type) {
	case nil, string:
		return x, nil
	case bool:
		if x {
			return int64(1), nil
		}
		return int64(0), nil
	case json.Number:
		if i, err := strconv.ParseInt(string(x), 10, 64); err == nil {
			return i, nil
		}
		f, err := strconv.ParseFloat(string(x), 64)
		if err != nil {
			return nil, badRequest("bangdb: invalid number " + string(x))
		}
		return f, nil
	case map[string]any:
		if s, ok := x["$blob"].(string); ok && len(x) == 1 {
			b, err := base64.StdEncoding.DecodeString(s)
			if err != nil {
				return nil, badRequest("bangdb: invalid $blob: " + err.Error())
			}
			return b, nil
		}
	}
	return nil, badRequest(fmt.Sprintf("bangdb: unsupported argument %v (use null, numbers, strings, booleans or {\"$blob\": base64})", v))
}

// toJSON converts an SQL value for a response.
func toJSON(v any) any {
	switch x := v.(type) {
	case []byte:
		return map[string]string{"$blob": base64.StdEncoding.EncodeToString(x)}
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil
		}
	}
	return v
}

func toJSONRow(row []any) []any {
	out := make([]any, len(row))
	for i, v := range row {
		out[i] = toJSON(v)
	}
	return out
}

// ---- endpoints ----

func (h *Handler) query(w http.ResponseWriter, r *http.Request) error {
	var st statement
	if err := decode(r, &st); err != nil {
		return err
	}
	ro, err := h.db.ReadOnlySQL(st.SQL)
	if err != nil {
		return err
	}
	if !ro {
		return badRequest("bangdb: /query runs SELECT and EXPLAIN only; use /exec for other statements")
	}
	args, err := st.args()
	if err != nil {
		return err
	}
	rows, err := h.db.QueryRows(r.Context(), st.SQL, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	// Rows are streamed: the response is written while the query runs, so
	// an error after the first row ends the JSON with an "error" member.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	put := func(prefix string, v any) error {
		b, err := json.Marshal(v)
		if err == nil {
			_, err = io.WriteString(w, prefix+string(b))
		}
		return err
	}
	put(`{"columns":`, rows.Columns())
	put(`,"types":`, rows.ColumnTypes())
	io.WriteString(w, `,"rows":[`)
	sep := ""
	for rows.Next() {
		if err := put(sep, toJSONRow(rows.Row())); err != nil {
			return nil // client gone
		}
		sep = ","
	}
	io.WriteString(w, "]")
	if err := rows.Err(); err != nil {
		put(`,"error":`, err.Error())
	}
	io.WriteString(w, "}\n")
	return nil
}

func execResult(res engine.Result) map[string]any {
	out := map[string]any{"rowsAffected": res.RowsAffected, "lastInsertId": res.LastInsertID}
	if len(res.Columns) > 0 {
		rows := make([][]any, len(res.Rows))
		for i, r := range res.Rows {
			rows[i] = toJSONRow(r)
		}
		out["columns"], out["rows"] = res.Columns, rows
	}
	return out
}

func (h *Handler) exec(w http.ResponseWriter, r *http.Request) error {
	if h.opts.ReadOnly {
		return errReadOnlyAPI
	}
	var st statement
	if err := decode(r, &st); err != nil {
		return err
	}
	args, err := st.args()
	if err != nil {
		return err
	}
	res, err := h.db.ExecContext(r.Context(), st.SQL, args...)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, execResult(res))
}

func (h *Handler) batch(w http.ResponseWriter, r *http.Request) error {
	if h.opts.ReadOnly {
		return errReadOnlyAPI
	}
	var req struct {
		Statements []statement `json:"statements"`
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	tx, err := h.db.BeginContext(r.Context())
	if err != nil {
		return err
	}
	results := make([]any, 0, len(req.Statements))
	for i, st := range req.Statements {
		args, err := st.args()
		if err == nil {
			var res engine.Result
			if res, err = tx.ExecContext(r.Context(), st.SQL, args...); err == nil {
				results = append(results, execResult(res))
				continue
			}
		}
		tx.Rollback()
		return &batchError{i, err}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// ---- errors ----

type apiError struct {
	status int
	code   string
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func badRequest(msg string) error { return &apiError{http.StatusBadRequest, "request", msg} }

var errReadOnlyAPI = &apiError{http.StatusForbidden, "readonly", "bangdb: this API is read-only"}

type batchError struct {
	index int
	err   error
}

func (e *batchError) Error() string { return fmt.Sprintf("statement %d: %v", e.index, e.err) }
func (e *batchError) Unwrap() error { return e.err }

func writeEngineError(w http.ResponseWriter, err error) {
	var ae *apiError
	if errors.As(err, &ae) {
		writeError(w, ae.status, ae.code, ae.msg)
		return
	}
	status, code := http.StatusBadRequest, "sql"
	switch {
	case errors.Is(err, bangdb.ErrConstraint):
		status, code = http.StatusConflict, "constraint"
	case errors.Is(err, bangdb.ErrBusy):
		status, code = http.StatusServiceUnavailable, "busy"
	case errors.Is(err, bangdb.ErrReadOnly):
		status, code = http.StatusForbidden, "readonly"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		status, code = 499, "interrupted"
	}
	var be *batchError
	if errors.As(err, &be) {
		writeJSON(w, status, map[string]any{"error": err.Error(), "code": code, "statement": be.index})
		return
	}
	writeError(w, status, code, err.Error())
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": msg, "code": code})
}

func writeJSON(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(v)
}
