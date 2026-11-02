package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfigPrecedence(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "bangdb.conf")
	os.WriteFile(file, []byte(`# BangDB settings
[server]
listen = ":7432"
http = :18080   # comment
query_timeout = 30s
cache_size = 64MB
busy_timeout = 2s
password_file = "`+filepath.Join(dir, "pw")+`"
`), 0o600)
	os.WriteFile(filepath.Join(dir, "pw"), []byte("s3cret\n"), 0o600)
	t.Setenv("BANGDB_BUSY_TIMEOUT", "3s")
	t.Setenv("BANGDB_HTTP", ":9999")

	var c config
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	c.register(fs)
	if err := fs.Parse([]string{"-http", ":8888", "app.db"}); err != nil {
		t.Fatal(err)
	}
	vals, err := readConfigFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.load(fs, vals); err != nil {
		t.Fatal(err)
	}
	if c.listen != ":7432" || c.httpAddr != ":8888" || c.busyTimeout != 3*time.Second ||
		c.queryTimeout != 30*time.Second || c.cacheSize != 64<<20 {
		t.Fatalf("effective settings:\n%s", c.describe())
	}
	if pw, err := secret(c.password, c.passwordFile); err != nil || pw != "s3cret" {
		t.Fatalf("password file: %q %v", pw, err)
	}
	if strings.Contains(c.describe(), "s3cret") {
		t.Fatal("describe shows a secret")
	}

	// Errors name their source.
	var c2 config
	fs2 := flag.NewFlagSet("t", flag.ContinueOnError)
	c2.register(fs2)
	fs2.Parse(nil)
	if err := c2.load(fs2, map[string]string{"cache_size": "lots"}); err == nil || !strings.Contains(err.Error(), "cache_size") {
		t.Fatalf("bad size: %v", err)
	}
	if err := c2.load(fs2, map[string]string{"nope": "1"}); err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("unknown key: %v", err)
	}
}

func TestEncryptionKeyFile(t *testing.T) {
	dir := t.TempDir()
	raw := bytes.Repeat([]byte{7}, 32)
	os.WriteFile(filepath.Join(dir, "raw.key"), raw, 0o600)
	os.WriteFile(filepath.Join(dir, "hex.key"), []byte(strings.Repeat("07", 32)+"\n"), 0o600)
	for _, f := range []string{"raw.key", "hex.key"} {
		c := config{encryptionKeyFile: filepath.Join(dir, f)}
		if k, err := c.key(); err != nil || !bytes.Equal(k, raw) {
			t.Errorf("%s: %x %v", f, k, err)
		}
	}
	c := config{encryptionKey: "zz"}
	if _, err := c.key(); err == nil {
		t.Error("non-hex key accepted")
	}
}
