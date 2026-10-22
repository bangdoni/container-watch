package auth_test

import (
	"strings"
	"testing"

	"github.com/containerwatch/containerwatch/internal/auth"
)

// TestHashPasswordRoundTrip checks that a password hashes and verifies.
func TestHashPasswordRoundTrip(t *testing.T) {
	const pw = "correct horse battery staple"
	hash, err := auth.HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Errorf("hash = %q, want an $argon2id$ prefix", hash)
	}
	ok, err := auth.VerifyPassword(pw, hash)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Error("VerifyPassword returned false for the correct password")
	}
}

// TestVerifyPasswordRejectsWrongPassword checks the negative path.
func TestVerifyPasswordRejectsWrongPassword(t *testing.T) {
	hash, err := auth.HashPassword("right-password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	ok, err := auth.VerifyPassword("wrong-password", hash)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if ok {
		t.Error("VerifyPassword accepted the wrong password")
	}
}

// TestHashPasswordIsSalted checks that two hashes of the same password
// differ, so identical passwords are not detectable from the database.
func TestHashPasswordIsSalted(t *testing.T) {
	a, err := auth.HashPassword("same-password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	b, err := auth.HashPassword("same-password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if a == b {
		t.Error("two hashes of the same password are identical — the salt is not random")
	}
	// Both must still verify.
	for _, h := range []string{a, b} {
		ok, err := auth.VerifyPassword("same-password", h)
		if err != nil || !ok {
			t.Errorf("hash %q did not verify (ok=%v err=%v)", h, ok, err)
		}
	}
}

// TestHashPasswordRejectsShortPassword checks the minimum length rule.
func TestHashPasswordRejectsShortPassword(t *testing.T) {
	if _, err := auth.HashPassword("short"); err == nil {
		t.Fatal("expected an error for a password under 8 characters")
	}
	// Exactly 8 is the boundary and must be accepted.
	if _, err := auth.HashPassword("12345678"); err != nil {
		t.Errorf("an 8-character password was rejected: %v", err)
	}
}

// TestVerifyPasswordRejectsMalformedHash checks that a corrupt hash returns
// an error rather than a false "verified" or a panic.
func TestVerifyPasswordRejectsMalformedHash(t *testing.T) {
	bad := []string{
		"",
		"not-a-hash",
		"$argon2id$",
		"$argon2id$v=19$m=65536,t=3,p=2$onlyonesalt",
		"$argon2id$v=99$m=65536,t=3,p=2$c2FsdA$a2V5",
	}
	for _, h := range bad {
		ok, err := auth.VerifyPassword("whatever", h)
		if err == nil && ok {
			t.Errorf("VerifyPassword(%q) accepted a malformed hash", h)
		}
	}
}

// TestSessionTokenIsRandomAndHashed checks the opaque session token
// contract: the raw token is returned once, only its hash is stored.
func TestSessionTokenIsRandomAndHashed(t *testing.T) {
	token, hash, err := auth.NewSessionToken()
	if err != nil {
		t.Fatalf("NewSessionToken: %v", err)
	}
	if token == "" || hash == "" {
		t.Fatal("NewSessionToken returned an empty token or hash")
	}
	if token == hash {
		t.Error("the raw token and its hash are identical — the token is stored in the clear")
	}
	if auth.HashToken(token) != hash {
		t.Error("HashToken(token) does not reproduce the returned hash")
	}
	if auth.HashToken(token) == token {
		t.Error("HashToken returned the token unchanged")
	}
}

// TestSessionTokensAreUnique checks the tokens do not collide.
func TestSessionTokensAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		token, _, err := auth.NewSessionToken()
		if err != nil {
			t.Fatalf("NewSessionToken: %v", err)
		}
		if seen[token] {
			t.Fatal("NewSessionToken produced a duplicate token")
		}
		seen[token] = true
	}
}

// TestRandomID checks the random id helper used for setup codes.
func TestRandomID(t *testing.T) {
	// RandomID(n) draws n random bytes and returns them hex-encoded, so
	// the result is 2n characters long.
	id := auth.RandomID(8)
	if len(id) != 16 {
		t.Errorf("RandomID(8) returned %d characters, want 16: %q", len(id), id)
	}
	if auth.RandomID(8) == id {
		t.Error("RandomID is not random")
	}
}
