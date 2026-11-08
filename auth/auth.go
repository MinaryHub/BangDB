// Package auth holds the user accounts of a BangDB server: names, roles
// and password hashes, read from a users file.
//
// Users file, one account per line (# starts a comment):
//
//	alice  admin  pbkdf2-sha256$100000$<salt>$<hash>
//	app    write  pbkdf2-sha256$100000$<salt>$<hash>
//	report read   pbkdf2-sha256$100000$<salt>$<hash>
//
// Create a line with "bangdb -add-user name:role" (it asks for the
// password) or HashPassword.
//
// Roles: read runs queries (SELECT, EXPLAIN, PRAGMA reads); write also
// changes data (INSERT, UPDATE, DELETE, transactions); admin also changes
// the schema and settings (CREATE/DROP/ALTER, VACUUM, ANALYZE, PRAGMA).
package auth

import (
	"bufio"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/minaryhub/bangdb/internal/engine"
)

// Role is what an account may do.
type Role int

const (
	RoleRead Role = iota
	RoleWrite
	RoleAdmin
)

func (r Role) String() string {
	switch r {
	case RoleRead:
		return "read"
	case RoleWrite:
		return "write"
	}
	return "admin"
}

// ParseRole parses read, write or admin.
func ParseRole(s string) (Role, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "read", "readonly", "reader":
		return RoleRead, nil
	case "write", "writer", "readwrite":
		return RoleWrite, nil
	case "admin":
		return RoleAdmin, nil
	}
	return 0, fmt.Errorf("auth: unknown role %q (read, write or admin)", s)
}

// Allows reports whether the role may run statements needing access.
func (r Role) Allows(access engine.Access) bool {
	switch access {
	case engine.AccessRead:
		return true
	case engine.AccessWrite:
		return r >= RoleWrite
	}
	return r == RoleAdmin
}

// ErrForbidden is returned when a role may not run a statement.
var ErrForbidden = errors.New("bangdb: permission denied")

// Check returns ErrForbidden (wrapped with an explanation) if role may not
// run src on db. Statements that do not parse are let through: running
// them reports the syntax error.
func Check(db *engine.DB, role Role, src string) error {
	need, err := db.SQLAccess(src)
	if err != nil || role.Allows(need) {
		return nil
	}
	what := "change data"
	if need == engine.AccessAdmin {
		what = "change the schema or settings"
	}
	return fmt.Errorf("%w: role %s may not %s", ErrForbidden, role, what)
}

// ---- passwords ----

const (
	hashScheme     = "pbkdf2-sha256"
	hashIterations = 100_000
)

// HashPassword returns a salted PBKDF2-SHA256 hash for a users file.
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := pbkdf2([]byte(password), salt, hashIterations, 32)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("%s$%d$%s$%s", hashScheme, hashIterations, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// verifyHash checks password against a hash made by HashPassword.
func verifyHash(hash, password string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != hashScheme {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 || iter > 10_000_000 {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err1 := enc.DecodeString(parts[2])
	want, err2 := enc.DecodeString(parts[3])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	return subtle.ConstantTimeCompare(pbkdf2([]byte(password), salt, iter, len(want)), want) == 1
}

// pbkdf2 is PBKDF2 (RFC 8018) with HMAC-SHA256.
func pbkdf2(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	var out []byte
	for block := 1; len(out) < keyLen; block++ {
		prf.Reset()
		prf.Write(salt)
		prf.Write([]byte{byte(block >> 24), byte(block >> 16), byte(block >> 8), byte(block)})
		u := prf.Sum(nil)
		t := append([]byte(nil), u...)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

// ---- accounts ----

// Users is a set of accounts. It is safe for concurrent use.
type Users struct {
	mu       sync.Mutex
	accounts map[string]account
	// Recently verified passwords, so that per-request authentication
	// (HTTP) does not pay the key derivation each time.
	verified map[[32]byte]time.Time
}

type account struct {
	role Role
	hash string
}

// NewUsers returns an empty set of accounts.
func NewUsers() *Users {
	return &Users{accounts: map[string]account{}, verified: map[[32]byte]time.Time{}}
}

// Add adds or replaces an account.
func (u *Users) Add(name string, role Role, passwordHash string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.accounts[name] = account{role, passwordHash}
	clear(u.verified)
}

// Len returns the number of accounts.
func (u *Users) Len() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.accounts)
}

// LoadUsers reads a users file.
func LoadUsers(path string) (*Users, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	u := NewUsers()
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("%s:%d: expected: name role hash", path, n)
		}
		role, err := ParseRole(fields[1])
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		if !strings.HasPrefix(fields[2], hashScheme+"$") {
			return nil, fmt.Errorf("%s:%d: the password must be a hash (bangdb -add-user), not plain text", path, n)
		}
		u.Add(fields[0], role, fields[2])
	}
	return u, sc.Err()
}

// dummyHash is verified for unknown users, so that their failures take as
// long as wrong passwords.
var dummyHash, _ = HashPassword("bangdb-dummy-password")

// Authenticate returns the role of name if password is right.
func (u *Users) Authenticate(name, password string) (Role, bool) {
	u.mu.Lock()
	acc, ok := u.accounts[name]
	u.mu.Unlock()
	if !ok {
		verifyHash(dummyHash, password)
		return 0, false
	}
	key := sha256.Sum256([]byte(name + "\x00" + password + "\x00" + acc.hash))
	u.mu.Lock()
	at, cached := u.verified[key]
	u.mu.Unlock()
	if cached && time.Since(at) < 5*time.Minute {
		return acc.role, true
	}
	if !verifyHash(acc.hash, password) {
		return 0, false
	}
	u.mu.Lock()
	if len(u.verified) > 10000 {
		clear(u.verified)
	}
	u.verified[key] = time.Now()
	u.mu.Unlock()
	return acc.role, true
}
