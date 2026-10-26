// Command bangdb is an interactive SQL shell for BangDB databases.
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
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/knoxxr/bangdb"
	"github.com/knoxxr/bangdb/client"
	"github.com/knoxxr/bangdb/internal/engine"
	"github.com/knoxxr/bangdb/internal/value"
	"github.com/knoxxr/bangdb/replication"
	"github.com/knoxxr/bangdb/server"
)

func main() {
	serve := flag.String("serve", "", "serve replicas on this address (primary mode)")
	replicaOf := flag.String("replica-of", "", "mirror the primary at this address (read-only replica)")
	token := flag.String("token", "", "replication token")
	listen := flag.String("listen", "", "serve the database to clients on this address (server mode)")
	password := flag.String("password", "", "password clients must present (server mode)")
	tlsCert := flag.String("tls-cert", "", "TLS certificate file (server mode)")
	tlsKey := flag.String("tls-key", "", "TLS key file (server mode)")
	connect := flag.String("connect", "", "run the shell against a server: bangdb://user:password@host:port")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: bangdb [-serve addr | -replica-of addr] [-token t] [-listen addr [-password p] [-tls-cert f -tls-key f]] [database] [sql...]")
		fmt.Fprintln(os.Stderr, "       bangdb -connect bangdb://user:password@host:port [sql...]")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *connect != "" {
		os.Exit(remoteShell(*connect, flag.Args()))
	}
	path := ":memory:"
	if flag.NArg() > 0 {
		path = flag.Arg(0)
	}
	pub, err := bangdb.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer pub.Close()
	db := pub.Engine()
	sh := &shell{db: db, conn: localSession{db.Conn()}, out: os.Stdout}
	if *listen != "" {
		srv := server.New(pub, server.Options{Password: *password})
		go func() {
			var err error
			if *tlsCert != "" {
				cert, cerr := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
				if cerr != nil {
					fmt.Fprintln(os.Stderr, "server:", cerr)
					os.Exit(1)
				}
				err = srv.ListenAndServeTLS(*listen, &tls.Config{Certificates: []tls.Certificate{cert}})
			} else {
				err = srv.ListenAndServe(*listen)
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, "server:", err)
				os.Exit(1)
			}
		}()
		defer srv.Close()
		fmt.Fprintf(os.Stderr, "serving clients on %s\n", *listen)
	}
	if *serve != "" {
		p := replication.NewPrimary(pub, replication.PrimaryOptions{Token: *token})
		go func() {
			if err := p.ListenAndServe(*serve); err != nil {
				fmt.Fprintln(os.Stderr, "replication:", err)
			}
		}()
		defer p.Close()
		sh.primary = p
		fmt.Fprintf(os.Stderr, "serving replicas on %s\n", *serve)
	}
	if *replicaOf != "" {
		r := replication.StartReplica(pub, *replicaOf, replication.ReplicaOptions{Token: *token})
		defer r.Stop()
		sh.replica = r
		fmt.Fprintf(os.Stderr, "replicating from %s (read-only)\n", *replicaOf)
	}
	if flag.NArg() > 1 {
		ok := sh.run(strings.Join(flag.Args()[1:], " "))
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
	if *serve != "" || *replicaOf != "" || *listen != "" {
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
	case ".quit", ".exit", ".help", ".timer":
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
