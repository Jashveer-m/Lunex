package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

var testSecret = []byte("test-secret-that-is-long-enough-32b")

func newTestIssuer(now func() time.Time) *TokenIssuer {
	t := NewTokenIssuer(testSecret, "lifeos-test", 15*time.Minute)
	if now != nil {
		t.now = now
	}
	return t
}

func TestIssueAndParse(t *testing.T) {
	issuer := newTestIssuer(nil)
	userID := uuid.New()

	token, expiresAt, err := issuer.Issue(userID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if d := time.Until(expiresAt); d < 14*time.Minute || d > 15*time.Minute {
		t.Fatalf("access token TTL = %v, want ~15m", d)
	}

	got, err := issuer.Parse(token)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got != userID {
		t.Fatalf("subject = %v, want %v", got, userID)
	}
}

func TestParseRejectsExpiredToken(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	issuer := newTestIssuer(func() time.Time { return base })
	token, _, err := issuer.Issue(uuid.New())
	if err != nil {
		t.Fatal(err)
	}

	// Same key, clock advanced past the 15 minute TTL.
	later := newTestIssuer(func() time.Time { return base.Add(16 * time.Minute) })
	if _, err := later.Parse(token); err == nil {
		t.Fatal("expired token was accepted")
	}
}

func TestParseRejectsWrongSecret(t *testing.T) {
	token, _, err := newTestIssuer(nil).Issue(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	other := NewTokenIssuer([]byte("a-completely-different-secret-key"), "lifeos-test", 15*time.Minute)
	if _, err := other.Parse(token); err == nil {
		t.Fatal("token signed with another key was accepted")
	}
}

func TestParseRejectsWrongIssuer(t *testing.T) {
	token, _, err := NewTokenIssuer(testSecret, "someone-else", 15*time.Minute).Issue(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newTestIssuer(nil).Parse(token); err == nil {
		t.Fatal("token from another issuer was accepted")
	}
}

// A token with "alg": "none" must never be trusted, even though it parses.
func TestParseRejectsAlgNone(t *testing.T) {
	claims := Claims{RegisteredClaims: jwt.RegisteredClaims{
		Subject:   uuid.NewString(),
		Issuer:    "lifeos-test",
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}}
	unsigned, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newTestIssuer(nil).Parse(unsigned); err == nil {
		t.Fatal(`token with alg "none" was accepted`)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	issuer := newTestIssuer(nil)
	for _, token := range []string{"", "not-a-jwt", "a.b.c", strings.Repeat("x", 500)} {
		if _, err := issuer.Parse(token); err == nil {
			t.Fatalf("garbage token %q was accepted", token)
		}
	}
}

func TestNewRefreshTokenIsUniqueAndHashed(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		token, hash, err := NewRefreshToken()
		if err != nil {
			t.Fatal(err)
		}
		if len(token) < 40 {
			t.Fatalf("refresh token is only %d chars: not enough entropy", len(token))
		}
		if seen[token] {
			t.Fatal("NewRefreshToken returned a duplicate")
		}
		seen[token] = true

		if hash == token {
			t.Fatal("stored hash equals the token itself")
		}
		if hash != HashRefreshToken(token) {
			t.Fatal("HashRefreshToken is not deterministic for the returned token")
		}
		if len(hash) != 64 {
			t.Fatalf("hash length = %d, want 64 hex chars", len(hash))
		}
	}
}
