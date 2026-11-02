// Command bangdb is an interactive SQL shell for BangDB databases, and
// runs them as servers. Settings come from flags, BANGDB_* environment
// variables or a configuration file (see config.go and -help).
//
//	bangdb app.db                         # interactive shell
//	bangdb app.db "SELECT 1"              # run SQL and exit
//	echo "SELECT 1;" | bangdb app.db
//	bangdb -serve :7070 app.db            # shell + serve replicas
//	bangdb -replica-of host:7070 r.db     # read-only mirror of a primary
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/knoxxr/bangdb"
	"github.com/knoxxr/bangdb/client"
	"github.com/knoxxr/bangdb/httpapi"
	"github.com/knoxxr/bangdb/internal/engine"
	"github.com/knoxxr/bangdb/internal/value"
	"github.com/knoxxr/bangdb/replication"
	"github.com/knoxxr/bangdb/server"
)

func main() {
	var cfg config
	fs := flag.CommandLine
	cfg.register(fs)
	printConfig := fs.Bool("print-config", false, "print the effective settings and exit")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: bangdb [options] [database] [sql...]
       bangdb -connect bangdb://user:password@host:port [sql...]

Every option can also be set in a configuration file (-config file, lines
"name = value" with underscores) or an environment variable BANGDB_<NAME>,
e.g. BANGDB_ENCRYPTION_KEY_FILE. Priority: flag > environment > file.

options:`)
		fs.PrintDefaults()
	}
	fs.Parse(os.Args[1:])
	var fileValues map[string]string
	if cfg.configFile != "" {
		var err error
		if fileValues, err = readConfigFile(cfg.configFile); err != nil {
			fatal(err)
		}
	}
	if err := cfg.load(fs, fileValues); err != nil {
		fatal(err)
	}
	if cfg.showVersion {
		fmt.Println("bangdb", bangdb.VersionString())
		return
	}
	if cfg.connect != "" {
		os.Exit(remoteShell(cfg.connect, fs.Args()))
	}
	args := fs.Args()
	if cfg.database == "" && len(args) > 0 {
		cfg.database, args = args[0], args[1:]
	} else if len(args) > 0 && args[0] == cfg.database {
		args = args[1:]
	}
	if cfg.database == "" {
		cfg.database = ":memory:"
	}
	serving := cfg.listen != "" || cfg.httpAddr != ""
	if serving && !isSet(fs, fileValues, "query_timeout") {
		cfg.queryTimeout = 60 * time.Second // one runaway query must not hold a server
	}
	if *printConfig {
		fmt.Println(cfg.describe())
		return
	}
	password, err := secret(cfg.password, cfg.passwordFile)
	if err != nil {
		fatal(err)
	}
	key, err := cfg.key()
	if err != nil {
		fatal(err)
	}
	path := cfg.database
	pub, err := bangdb.OpenWithOptions(path, &bangdb.Options{
		BusyTimeout: cfg.busyTimeout, QueryTimeout: cfg.queryTimeout, SlowQueryThreshold: cfg.slowQuery,
		CacheSize: int(cfg.cacheSize), SortMemory: int(cfg.sortMemory), TempDir: cfg.tempDir, EncryptionKey: key,
	})
	if err != nil {
		fatal(err)
	}
	defer pub.Close()
	db := pub.Engine()
	sh := &shell{db: db, conn: localSession{db.Conn()}, out: os.Stdout}
	if serving && password == "" {
		fmt.Fprintln(os.Stderr, "warning: serving without a password; anyone who can reach the port has full access")
	}
	var tlsConfig *tls.Config
	if cfg.tlsCert != "" {
		cert, err := tls.LoadX509KeyPair(cfg.tlsCert, cfg.tlsKey)
		if err != nil {
			fatal(err)
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	}
	if cfg.listen != "" {
		srv := server.New(pub, server.Options{Password: password, MaxConns: cfg.maxConns, IdleTimeout: cfg.idleTimeout})
		go func() {
			var err error
			if tlsConfig != nil {
				err = srv.ListenAndServeTLS(cfg.listen, tlsConfig)
			} else {
				err = srv.ListenAndServe(cfg.listen)
			}
			if err != nil {
				fatal(fmt.Errorf("server: %w", err))
			}
		}()
		defer srv.Close()
		fmt.Fprintf(os.Stderr, "serving clients on %s\n", cfg.listen)
	}
	if cfg.httpAddr != "" {
		var origins []string
		for _, o := range strings.Split(cfg.httpCORS, ",") {
			if o = strings.TrimSpace(o); o != "" {
				origins = append(origins, o)
			}
		}
		hs := &http.Server{
			Addr: cfg.httpAddr, TLSConfig: tlsConfig,
			Handler: httpapi.New(pub, httpapi.Options{Token: password, AllowOrigins: origins,
				ReadOnly: cfg.httpReadOnly, MaxConcurrent: cfg.httpMaxConcurrent}),
			// Slow or idle clients must not hold connections forever.
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       60 * time.Second,
			IdleTimeout:       2 * time.Minute,
		}
		go func() {
			var err error
			if tlsConfig != nil {
				err = hs.ListenAndServeTLS("", "")
			} else {
				err = hs.ListenAndServe()
			}
			if err != nil && err != http.ErrServerClosed {
				fatal(fmt.Errorf("http: %w", err))
			}
		}()
		defer hs.Close()
		fmt.Fprintf(os.Stderr, "serving the HTTP API on %s\n", cfg.httpAddr)
	}
	if cfg.serve != "" {
		p := replication.NewPrimary(pub, replication.PrimaryOptions{Token: cfg.token})
		go func() {
			if err := p.ListenAndServe(cfg.serve); err != nil {
				fmt.Fprintln(os.Stderr, "replication:", err)
			}
		}()
		defer p.Close()
		sh.primary = p
		fmt.Fprintf(os.Stderr, "serving replicas on %s\n", cfg.serve)
	}
	if cfg.replicaOf != "" {
		r := replication.StartReplica(pub, cfg.replicaOf, replication.ReplicaOptions{Token: cfg.token})
		defer r.Stop()
		sh.replica = r
		fmt.Fprintf(os.Stderr, "replicating from %s (read-only)\n", cfg.replicaOf)
	}
	if len(args) > 0 {
		ok := sh.run(strings.Join(args, " "))
		sh.conn.Close()
		if !ok {
			os.Exit(1)
		}
		return
	}
	interactive := isTerminal()
	if interactive {
		fmt.Printf("BangDB %s — %s\nEnter \".help\" for usage hints.\n", bangdb.Version, path)
	}
	sh.loop(os.Stdin, interactive)
	sh.conn.Close()
	if serving || cfg.serve != "" || cfg.replicaOf != "" {
		// Keep replicating after stdin is closed until interrupted.
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
	}
}

func isTerminal() bool {
	st, err := os.Stdin.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// session runs SQL for the shell: locally or on a server.
type session interface {
	Exec(src string) (engine.Result, error)
	InTx() bool
	Close() error
}

type localSession struct{ c *engine.Conn }

func (l localSession) Exec(src string) (engine.Result, error) { return l.c.Exec(src) }
func (l localSession) InTx() bool                             { return l.c.InTx() }
func (l localSession) Close() error                           { return l.c.Close() }

type remoteSession struct{ c *client.Conn }

func (r remoteSession) Exec(src string) (engine.Result, error) {
	res, err := r.c.Exec(context.Background(), src)
	return engine.Result{Columns: res.Columns, Types: res.Types, Rows: res.Rows,
		RowsAffected: res.RowsAffected, LastInsertID: res.LastInsertID}, err
}
func (r remoteSession) InTx() bool   { return r.c.InTx() }
func (r remoteSession) Close() error { return r.c.Close() }

// remoteShell runs the shell (SQL only) against a server.
func remoteShell(dsn string, args []string) int {
	addr, opts, err := client.ParseDSN(dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	c, err := client.Dial(context.Background(), addr, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer c.Close()
	sh := &shell{conn: remoteSession{c}, out: os.Stdout}
	if len(args) > 0 {
		if !sh.run(strings.Join(args, " ")) {
			return 1
		}
		return 0
	}
	interactive := isTerminal()
	if interactive {
		fmt.Printf("BangDB shell — %s\nEnter \".help\" for usage hints.\n", addr)
	}
	sh.loop(os.Stdin, interactive)
	return 0
}

type shell struct {
	db      *engine.DB // nil for a remote shell
	conn    session
	out     io.Writer
	timer   bool
	primary *replication.Primary
	replica *replication.Replica
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
	case ".quit", ".exit", ".help", ".timer", ".version":
	default:
		if s.db == nil {
			fmt.Fprintf(s.out, "%s is not available on a remote connection\n", fields[0])
			return true
		}
	}
	switch fields[0] {
	case ".quit", ".exit":
		return false
	case ".help":
		fmt.Fprintln(s.out, `.tables            list tables and views
.schema [table]    show table definitions
.timer on|off      show execution time
.backup FILE       write an online backup to FILE
.check             verify checksums, structure and indexes
.version           show the BangDB version
.stats             show database statistics
.dump [FILE]       write the database as SQL (to FILE or the screen)
.read FILE         import an SQL script or dump (also SQLite .dump output)
.import FILE TABLE import CSV with a header row (creates TABLE if missing)
.export FILE QUERY write the result of QUERY as CSV
.repl              show replication status
.promote           turn this replica into a writable primary
.quit              exit
SQL statements end with ';'. BEGIN/COMMIT/ROLLBACK are supported.`)
	case ".tables":
		names := append(s.db.Catalog().TableNames(), s.db.Catalog().ViewNames()...)
		sort.Strings(names)
		fmt.Fprintln(s.out, strings.Join(names, "  "))
	case ".schema":
		cat := s.db.Catalog()
		names := append(cat.TableNames(), cat.ViewNames()...)
		sort.Strings(names)
		for _, n := range names {
			if len(fields) > 1 && !strings.EqualFold(fields[1], n) {
				continue
			}
			if d := cat.Describe(n); d != "" {
				fmt.Fprintln(s.out, d)
			} else {
				fmt.Fprintln(s.out, cat.DescribeView(n))
			}
		}
	case ".backup":
		if len(fields) < 2 {
			fmt.Fprintln(s.out, "usage: .backup FILE")
			break
		}
		if err := s.db.Backup(context.Background(), fields[1]); err != nil {
			fmt.Fprintln(s.out, "Error:", err)
		} else {
			fmt.Fprintf(s.out, "backup written to %s (seq %d)\n", fields[1], s.db.Seq())
		}
	case ".check":
		problems, err := s.db.IntegrityCheck(context.Background())
		switch {
		case err != nil:
			fmt.Fprintln(s.out, "Error:", err)
		case len(problems) == 0:
			fmt.Fprintln(s.out, "ok")
		default:
			fmt.Fprintln(s.out, strings.Join(problems, "\n"))
		}
	case ".repl":
		fmt.Fprintf(s.out, "seq %d, term %d, read-only %v\n", s.db.Seq(), s.db.Term(), s.db.ReadOnly())
		if s.primary != nil {
			for _, r := range s.primary.Replicas() {
				fmt.Fprintf(s.out, "replica %s: sent %d, applied %d, lag %d commits (%s), snapshots %d, since %s\n",
					r.Addr, r.Seq, r.AckSeq, r.Lag, r.LagTime.Round(time.Millisecond), r.Snapshots, r.Since.Format(time.RFC3339))
			}
		}
		if s.replica != nil {
			fmt.Fprintf(s.out, "replica status: %+v\n", s.replica.Status())
		}
	case ".promote":
		if s.replica == nil {
			fmt.Fprintln(s.out, "not a replica")
			break
		}
		if err := s.replica.Promote(); err != nil {
			fmt.Fprintln(s.out, "Error:", err)
		} else {
			s.replica = nil
			fmt.Fprintln(s.out, "promoted: database is now writable")
		}
	case ".dump":
		w := io.Writer(s.out)
		if len(fields) > 1 {
			f, err := os.Create(fields[1])
			if err != nil {
				fmt.Fprintln(s.out, "Error:", err)
				break
			}
			defer f.Close()
			w = f
		}
		if err := s.db.Dump(context.Background(), w); err != nil {
			fmt.Fprintln(s.out, "Error:", err)
		}
	case ".read":
		if len(fields) < 2 {
			fmt.Fprintln(s.out, "usage: .read FILE")
			break
		}
		f, err := os.Open(fields[1])
		if err != nil {
			fmt.Fprintln(s.out, "Error:", err)
			break
		}
		res, err := s.db.ImportSQL(context.Background(), f)
		f.Close()
		if err != nil {
			fmt.Fprintln(s.out, "Error:", err)
			break
		}
		fmt.Fprintf(s.out, "%d statements, %d rows imported\n", res.Statements, res.Rows)
		for _, sk := range res.Skipped {
			fmt.Fprintln(s.out, "skipped:", sk)
		}
	case ".import":
		if len(fields) < 3 {
			fmt.Fprintln(s.out, "usage: .import FILE TABLE")
			break
		}
		f, err := os.Open(fields[1])
		if err != nil {
			fmt.Fprintln(s.out, "Error:", err)
			break
		}
		n, err := s.db.ImportCSV(context.Background(), fields[2], f, &engine.CSVOptions{Create: true})
		f.Close()
		if err != nil {
			fmt.Fprintln(s.out, "Error:", err)
		} else {
			fmt.Fprintf(s.out, "%d rows imported\n", n)
		}
	case ".export":
		parts := strings.SplitN(strings.TrimSpace(cmd), " ", 3)
		if len(parts) < 3 {
			fmt.Fprintln(s.out, "usage: .export FILE QUERY")
			break
		}
		f, err := os.Create(parts[1])
		if err != nil {
			fmt.Fprintln(s.out, "Error:", err)
			break
		}
		n, err := s.db.ExportCSV(context.Background(), f, nil, strings.TrimSuffix(strings.TrimSpace(parts[2]), ";"))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			fmt.Fprintln(s.out, "Error:", err)
		} else {
			fmt.Fprintf(s.out, "%d rows exported\n", n)
		}
	case ".stats":
		st := s.db.Stats()
		hit := func(h, m uint64) string {
			if h+m == 0 {
				return "-"
			}
			return fmt.Sprintf("%.1f%%", 100*float64(h)/float64(h+m))
		}
		avg := func(d time.Duration, n uint64) time.Duration {
			if n == 0 {
				return 0
			}
			return d / time.Duration(n)
		}
		fmt.Fprintf(s.out, "statements  %d (queries %d, errors %d, slow %d)\n", st.Statements, st.Queries, st.Errors, st.SlowQueries)
		fmt.Fprintf(s.out, "commits     %d (avg %s, rollbacks %d, WAL fsyncs %d)\n", st.TxCommits, avg(st.CommitTime, st.TxCommits), st.TxRollbacks, st.WALSyncs)
		fmt.Fprintf(s.out, "lock waits  %d (total %s, busy errors %d)\n", st.LockWaits, st.LockWaitTime, st.BusyErrors)
		fmt.Fprintf(s.out, "WAL         %d pages, %d bytes; checkpoints %d (%d pages, %s)\n", st.WALPages, st.WALBytes, st.Checkpoints, st.CheckpointPages, st.CheckpointTime)
		fmt.Fprintf(s.out, "cache       pages %s (%d disk reads), nodes %s\n", hit(st.PageCacheHits, st.PageReads), st.PageReads, hit(st.NodeHits, st.NodeMisses))
		fmt.Fprintf(s.out, "snapshots   %d open (lag %d commits), cursors %d; seq %d, %d pages\n", st.Snapshots, st.SnapshotLag, st.OpenCursors, st.Seq, st.PageCount)
	case ".version":
		fmt.Fprintln(s.out, "bangdb", bangdb.VersionString())
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

func fatal(err error) {
	msg := err.Error()
	if !strings.HasPrefix(msg, "bangdb:") {
		msg = "bangdb: " + msg
	}
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}

// isSet reports whether a setting was given by flag, environment or file.
func isSet(fs *flag.FlagSet, file map[string]string, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == strings.ReplaceAll(name, "_", "-") })
	_, inEnv := os.LookupEnv("BANGDB_" + strings.ToUpper(name))
	_, inFile := file[name]
	return set || inEnv || inFile
}
