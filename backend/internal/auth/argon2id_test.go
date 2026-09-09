package auth

import (
	"strings"
	"testing"
)

// testParams keep unit tests fast; production uses DefaultArgon2Params.
func testParams() Argon2Params {
	p := DefaultArgon2Params()
	p.Memory = 8 * 1024
	p.Iterations = 1
	return p
}

func TestHashPasswordFormat(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple", testParams())
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=8192,t=1,p=1$") {
		t.Fatalf("unexpected hash prefix: %q", hash)
	}
	if got := len(strings.Split(hash, "$")); got != 6 {
		t.Fatalf("want 6 PHC segments, got %d in %q", got, hash)
	}
}

func TestHashPasswordIsSalted(t *testing.T) {
	a, err := HashPassword("same-password-123", testParams())
	if err != nil {
		t.Fatal(err)
	}
	b, err := HashPassword("same-password-123", testParams())
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two hashes of the same password are identical: salt is not random")
	}
}

func TestVerifyPassword(t *testing.T) {
	const password = "correct horse battery staple"
	hash, err := HashPassword(password, testParams())
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		password string
		want     bool
	}{
		{"exact match", password, true},
		{"wrong password", "Correct horse battery staple", false},
		{"empty", "", false},
		{"prefix of the real password", password[:10], false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := VerifyPassword(tc.password, hash)
			if err != nil {
				t.Fatalf("VerifyPassword: %v", err)
			}
			if ok != tc.want {
				t.Fatalf("VerifyPassword(%q) = %v, want %v", tc.password, ok, tc.want)
			}
		})
	}
}

func TestVerifyPasswordRejectsMalformedHash(t *testing.T) {
	valid, err := HashPassword("password-1234", testParams())
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]string{
		"empty":            "",
		"plaintext":        "password-1234",
		"bcrypt":           "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy",
		"wrong algorithm":  strings.Replace(valid, "argon2id", "argon2i", 1),
		"wrong version":    strings.Replace(valid, "v=19", "v=16", 1),
		"truncated":        valid[:len(valid)-10],
		"missing segments": "$argon2id$v=19$m=8192,t=1,p=1",
	}
	for name, hash := range tests {
		t.Run(name, func(t *testing.T) {
			ok, err := VerifyPassword("password-1234", hash)
			if ok {
				t.Fatalf("malformed hash %q verified successfully", hash)
			}
			if err == nil {
				t.Fatal("want an error for a malformed hash, got nil")
			}
		})
	}
}

func TestDecodeHashRoundTripsParams(t *testing.T) {
	want := testParams()
	hash, err := HashPassword("password-1234", want)
	if err != nil {
		t.Fatal(err)
	}
	got, salt, key, err := decodeHash(hash)
	if err != nil {
		t.Fatal(err)
	}
	if got.Memory != want.Memory || got.Iterations != want.Iterations || got.Parallelism != want.Parallelism {
		t.Fatalf("params round-trip: got %+v, want %+v", got, want)
	}
	if uint32(len(salt)) != want.SaltLength {
		t.Fatalf("salt length = %d, want %d", len(salt), want.SaltLength)
	}
	if uint32(len(key)) != want.KeyLength {
		t.Fatalf("key length = %d, want %d", len(key), want.KeyLength)
	}
}
