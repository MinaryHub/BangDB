package main

import (
	"bufio"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// config holds the CLI's settings. Each comes, in increasing priority,
// from its default, the configuration file (-config, or BANGDB_CONFIG), an
// environment variable BANGDB_<NAME> and a command-line flag -<name>.
//
// The file has one "name = value" per line (a TOML subset): values may be
// quoted, # starts a comment, names use underscores (flags use dashes).
type config struct {
	database string

	listen, httpAddr, httpCORS string
	httpReadOnly               bool
	httpMaxConcurrent          int
	password, passwordFile     string
	tlsCert, tlsKey            string
	maxConns                   int
	idleTimeout                time.Duration

	serve, replicaOf, token string

	encryptionKey, encryptionKeyFile string
	busyTimeout, queryTimeout        time.Duration
	slowQuery                        time.Duration
	cacheSize, sortMemory, txMemory  int64
	tempDir                          string

	usersFile           string
	logFormat, logLevel string
	backupDir           string
	backupInterval      time.Duration
	backupKeep          int

	history string

	connect     string
	configFile  string
	showVersion bool
	addUser     string
}

type setting struct {
	name  string // snake_case; flag: dashes, env: BANGDB_UPPER
	usage string
	ptr   any // *string, *bool, *int, *int64 (size), *time.Duration
}

func (c *config) settings() []setting {
	return []setting{
		{"database", "database file (or the first argument; default :memory:)", &c.database},
		{"listen", "serve clients (binary protocol) on this address, e.g. :7432", &c.listen},
		{"http", "serve the HTTP/JSON API on this address, e.g. :18080", &c.httpAddr},
		{"http_cors", "comma-separated origins allowed to call the HTTP API from browsers (* = any)", &c.httpCORS},
		{"http_readonly", "HTTP API: allow only /query", &c.httpReadOnly},
		{"http_max_concurrent", "HTTP API: requests run at once (others wait)", &c.httpMaxConcurrent},
		{"password", "password clients must present (server and HTTP)", &c.password},
		{"password_file", "file holding the password (keeps it out of process listings)", &c.passwordFile},
		{"tls_cert", "TLS certificate file (server and HTTP)", &c.tlsCert},
		{"tls_key", "TLS key file", &c.tlsKey},
		{"max_conns", "server: maximum client connections (0 = unlimited)", &c.maxConns},
		{"idle_timeout", "server: close connections idle this long (0 = never)", &c.idleTimeout},
		{"serve", "serve replicas on this address (primary)", &c.serve},
		{"replica_of", "mirror the primary at this address (read-only replica)", &c.replicaOf},
		{"token", "replication token", &c.token},
		{"encryption_key", "encryption key as hex (64 digits for 32 bytes); prefer encryption_key_file", &c.encryptionKey},
		{"encryption_key_file", "file holding the encryption key (raw bytes, or hex text)", &c.encryptionKeyFile},
		{"busy_timeout", "how long a writer waits for the write lock", &c.busyTimeout},
		{"query_timeout", "limit for every statement (default 60s when serving clients; 0 = none)", &c.queryTimeout},
		{"slow_query", "log statements slower than this to stderr (0 = off)", &c.slowQuery},
		{"cache_size", "page and node cache, e.g. 64MB", &c.cacheSize},
		{"sort_memory", "memory per query for sorting before spilling to disk, e.g. 128MB", &c.sortMemory},
		{"tx_memory", "memory per write transaction for changed pages before they spill to disk, e.g. 64MB", &c.txMemory},
		{"temp_dir", "directory for spill files", &c.tempDir},
		{"users_file", "accounts with roles for the server and HTTP API (see -add-user)", &c.usersFile},
		{"log_format", "server log format: text or json", &c.logFormat},
		{"log_level", "server log level: debug, info, warn or error", &c.logLevel},
		{"backup_dir", "take scheduled, verified backups into this directory", &c.backupDir},
		{"backup_interval", "time between scheduled backups (default 24h)", &c.backupInterval},
		{"backup_keep", "number of scheduled backups kept (0 = all)", &c.backupKeep},
		{"history", "file keeping the lines entered in the shell (default: .bangdb_history in the home directory; off = none)", &c.history},
	}
}

// load fills c from defaults, the configuration file, the environment and
// the flags (already parsed into fs).
func (c *config) load(fs *flag.FlagSet, fileValues map[string]string) error {
	setFlags := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { setFlags[strings.ReplaceAll(f.Name, "-", "_")] = true })
	for _, s := range c.settings() {
		if setFlags[s.name] {
			continue // the flag already set it
		}
		v, ok := os.LookupEnv("BANGDB_" + strings.ToUpper(s.name))
		src := "environment variable BANGDB_" + strings.ToUpper(s.name)
		if !ok {
			v, ok = fileValues[s.name]
			src = "configuration file key " + s.name
		}
		if !ok {
			continue
		}
		if err := setValue(s.ptr, v); err != nil {
			return fmt.Errorf("%s: %w", src, err)
		}
	}
	for k := range fileValues {
		known := false
		for _, s := range c.settings() {
			known = known || s.name == k
		}
		if !known {
			return fmt.Errorf("configuration file: unknown key %q", k)
		}
	}
	return nil
}

// register defines a flag per setting (with its current value as default).
func (c *config) register(fs *flag.FlagSet) {
	for _, s := range c.settings() {
		name := strings.ReplaceAll(s.name, "_", "-")
		switch p := s.ptr.(type) {
		case *string:
			fs.StringVar(p, name, *p, s.usage)
		case *bool:
			fs.BoolVar(p, name, *p, s.usage)
		case *int:
			fs.IntVar(p, name, *p, s.usage)
		case *time.Duration:
			fs.DurationVar(p, name, *p, s.usage)
		case *int64:
			fs.Var((*sizeValue)(p), name, s.usage)
		}
	}
	fs.StringVar(&c.configFile, "config", os.Getenv("BANGDB_CONFIG"), "configuration file (name = value lines)")
	fs.StringVar(&c.connect, "connect", "", "run the shell against a server: bangdb://user:password@host:port")
	fs.BoolVar(&c.showVersion, "version", false, "print the version and exit")
	fs.StringVar(&c.addUser, "add-user", "", "name:role — read a password and print (or append to users_file) an account line")
}

func setValue(ptr any, v string) error {
	switch p := ptr.(type) {
	case *string:
		*p = v
	case *bool:
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("%q is not true or false", v)
		}
		*p = b
	case *int:
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("%q is not a number", v)
		}
		*p = n
	case *time.Duration:
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("%q is not a duration (e.g. 30s, 5m)", v)
		}
		*p = d
	case *int64:
		return (*sizeValue)(p).Set(v)
	}
	return nil
}

// sizeValue parses byte sizes such as 64MB, 1GiB or 1048576.
type sizeValue int64

func (s *sizeValue) String() string { return strconv.FormatInt(int64(*s), 10) }

func (s *sizeValue) Set(v string) error {
	v = strings.TrimSpace(strings.ToUpper(v))
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		m      int64
	}{{"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(v, u.suffix) {
			v, mult = strings.TrimSpace(strings.TrimSuffix(v, u.suffix)), u.m
			break
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return fmt.Errorf("%q is not a size (e.g. 64MB)", v)
	}
	*s = sizeValue(n * mult)
	return nil
}

// readConfigFile parses a configuration file.
func readConfigFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == '[' {
			continue // comments; [sections] are ignored
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected name = value", path, n)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
			end := strings.IndexByte(v[1:], v[0])
			if end < 0 {
				return nil, fmt.Errorf("%s:%d: unterminated quote", path, n)
			}
			v = v[1 : end+1]
		} else if i := strings.Index(v, " #"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		out[strings.ReplaceAll(k, "-", "_")] = v
	}
	return out, sc.Err()
}

// secret returns the value of a setting that may come from a file.
func secret(value, file string) (string, error) {
	if file == "" {
		return value, nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

// encryptionKey decodes the configured key: hex text, or raw bytes from a
// file.
func (c *config) key() ([]byte, error) {
	if c.encryptionKeyFile != "" {
		b, err := os.ReadFile(c.encryptionKeyFile)
		if err != nil {
			return nil, err
		}
		if t := strings.TrimSpace(string(b)); len(t) >= 32 && isHex(t) {
			return hex.DecodeString(t)
		}
		return b, nil
	}
	if c.encryptionKey == "" {
		return nil, nil
	}
	k, err := hex.DecodeString(strings.TrimSpace(c.encryptionKey))
	if err != nil {
		return nil, fmt.Errorf("encryption_key must be hex: %w", err)
	}
	return k, nil
}

func isHex(s string) bool {
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}

// describe lists the effective settings (secrets masked), for -print-config.
func (c *config) describe() string {
	var lines []string
	for _, s := range c.settings() {
		var v string
		switch p := s.ptr.(type) {
		case *string:
			v = *p
		case *bool:
			v = strconv.FormatBool(*p)
		case *int:
			v = strconv.Itoa(*p)
		case *time.Duration:
			v = p.String()
		case *int64:
			v = strconv.FormatInt(*p, 10)
		}
		if (s.name == "password" || s.name == "encryption_key" || s.name == "token") && v != "" {
			v = "********"
		}
		lines = append(lines, fmt.Sprintf("%s = %q", s.name, v))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
