package auth

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/minaryhub/bangdb/internal/engine"
)

// RFC 7914 section 11 test vector for PBKDF2-HMAC-SHA256.
func TestPBKDF2(t *testing.T) {
	got := hex.EncodeToString(pbkdf2([]byte("passwd"), []byte("salt"), 1, 64))
	want := "55ac046e56e3089fec1691c22544b605f94185216dde0465e68b9d57c20dacbc49ca9cccf179b645991664b39d77ef317c71b845b1e30bd509112041d3a19783"
	if got != want {
		t.Fatalf("pbkdf2 = %s", got)
	}
}

func TestUsers(t *testing.T) {
	h1, _ := HashPassword("pw-admin")
	h2, _ := HashPassword("pw-read")
	path := filepath.Join(t.TempDir(), "users")
	os.WriteFile(path, []byte("# accounts\nalice admin "+h1+"\nbob  read  "+h2+"\n"), 0o600)
	u, err := LoadUsers(path)
	if err != nil || u.Len() != 2 {
		t.Fatal(u, err)
	}
	if r, ok := u.Authenticate("alice", "pw-admin"); !ok || r != RoleAdmin {
		t.Error("alice")
	}
	if r, ok := u.Authenticate("bob", "pw-read"); !ok || r != RoleRead {
		t.Error("bob")
	}
	if _, ok := u.Authenticate("bob", "pw-admin"); ok {
		t.Error("wrong password accepted")
	}
	if _, ok := u.Authenticate("carol", "x"); ok {
		t.Error("unknown user accepted")
	}
	if r, ok := u.Authenticate("bob", "pw-read"); !ok || r != RoleRead { // cached path
		t.Error("cached verification")
	}
	os.WriteFile(path, []byte("eve admin plaintext\n"), 0o600)
	if _, err := LoadUsers(path); err == nil || !strings.Contains(err.Error(), "hash") {
		t.Errorf("plain password accepted: %v", err)
	}
	if RoleRead.Allows(engine.AccessWrite) || !RoleWrite.Allows(engine.AccessWrite) || RoleWrite.Allows(engine.AccessAdmin) || !RoleAdmin.Allows(engine.AccessAdmin) {
		t.Error("Allows")
	}
}
