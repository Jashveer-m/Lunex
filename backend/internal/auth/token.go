package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// ErrInvalidToken covers every reason an access token was rejected. The caller
// never needs the distinction, and leaking it helps an attacker.
var ErrInvalidToken = errors.New("invalid or expired token")

// Claims is the access-token payload. Subject holds the user id.
type Claims struct {
	jwt.RegisteredClaims
}

// TokenIssuer mints and verifies short-lived access tokens (HS256).
type TokenIssuer struct {
	secret    []byte
	issuer    string
	accessTTL time.Duration
	now       func() time.Time // injectable for tests
}

func NewTokenIssuer(secret []byte, issuer string, accessTTL time.Duration) *TokenIssuer {
	return &TokenIssuer{secret: secret, issuer: issuer, accessTTL: accessTTL, now: time.Now}
}

// Issue returns a signed access token for userID and its expiry.
func (t *TokenIssuer) Issue(userID uuid.UUID) (string, time.Time, error) {
	now := t.now()
	expiresAt := now.Add(t.accessTTL)
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			Issuer:    t.issuer,
			ID:        uuid.NewString(),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign access token: %w", err)
	}
	return signed, expiresAt, nil
}

// Parse validates the signature, expiry and issuer, and returns the user id.
func (t *TokenIssuer) Parse(token string) (uuid.UUID, error) {
	parsed, err := jwt.ParseWithClaims(token, &Claims{},
		func(tok *jwt.Token) (any, error) {
			// Pin the algorithm: without this, "alg": "none" and RS256-key
			// confusion attacks both become possible.
			if _, ok := tok.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method %v", tok.Header["alg"])
			}
			return t.secret, nil
		},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(t.issuer),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(t.now),
	)
	if err != nil || !parsed.Valid {
		return uuid.Nil, ErrInvalidToken
	}
	claims, ok := parsed.Claims.(*Claims)
	if !ok {
		return uuid.Nil, ErrInvalidToken
	}
	id, err := uuid.Parse(claims.Subject)
	if err != nil {
		return uuid.Nil, ErrInvalidToken
	}
	return id, nil
}

// refreshTokenBytes is the entropy of an opaque refresh token.
const refreshTokenBytes = 32

// NewRefreshToken returns a fresh opaque token (URL-safe base64) and the hash
// that gets persisted.
func NewRefreshToken() (token string, hash string, err error) {
	buf := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generate refresh token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(buf)
	return token, HashRefreshToken(token), nil
}

// HashRefreshToken is the one-way function applied before a refresh token
// touches the database. SHA-256 (not argon2id) is deliberate: the token is 256
// bits of uniform randomness, so there is no dictionary to grind, and refresh
// happens on a hot path where a memory-hard KDF would be a DoS lever.
func HashRefreshToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
