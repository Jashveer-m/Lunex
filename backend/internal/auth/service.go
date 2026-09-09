package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/users"
)

var (
	// ErrInvalidCredentials is returned for both an unknown email and a wrong
	// password: the caller must not be able to tell those apart.
	ErrInvalidCredentials = errors.New("invalid email or password")
	// ErrEmailTaken is returned when registration hits an existing account.
	ErrEmailTaken = users.ErrEmailTaken
)

// UserStore is the slice of the users repository the auth service needs.
type UserStore interface {
	Create(ctx context.Context, email, passwordHash, name, timezone string) (users.User, users.Profile, error)
	ByEmail(ctx context.Context, email string) (users.User, error)
	ByID(ctx context.Context, id uuid.UUID) (users.User, error)
	ProfileByUserID(ctx context.Context, id uuid.UUID) (users.Profile, error)
}

// SessionStore is the slice of the session repository the auth service needs.
type SessionStore interface {
	Create(ctx context.Context, userID uuid.UUID, hash string, expiresAt time.Time, deviceInfo *string) (Session, error)
	Rotate(ctx context.Context, oldHash, newHash string, expiresAt time.Time) (Session, error)
	DeleteByTokenHash(ctx context.Context, hash string) error
}

// Service holds the auth use cases. It is transport agnostic.
type Service struct {
	users      UserStore
	sessions   SessionStore
	tokens     *TokenIssuer
	hashParams Argon2Params
	refreshTTL time.Duration
	now        func() time.Time
}

func NewService(u UserStore, s SessionStore, tokens *TokenIssuer, refreshTTL time.Duration) *Service {
	return &Service{
		users:      u,
		sessions:   s,
		tokens:     tokens,
		hashParams: DefaultArgon2Params(),
		refreshTTL: refreshTTL,
		now:        time.Now,
	}
}

type RegisterInput struct {
	Email      string
	Password   string
	Name       string
	Timezone   string
	DeviceInfo string
}

type LoginInput struct {
	Email      string
	Password   string
	DeviceInfo string
}

// TokenPair is what a successful register/login/refresh hands back.
type TokenPair struct {
	AccessToken     string
	AccessExpiresAt time.Time
	RefreshToken    string
	RefreshExpires  time.Time
	SessionID       uuid.UUID
}

// Account is a user plus their profile, as returned by GET /me.
type Account struct {
	User    users.User
	Profile users.Profile
}

// Register creates the user, its profile and a first session.
func (s *Service) Register(ctx context.Context, in RegisterInput) (Account, TokenPair, error) {
	if err := ValidateRegister(in); err != nil {
		return Account{}, TokenPair{}, err
	}

	hash, err := HashPassword(in.Password, s.hashParams)
	if err != nil {
		return Account{}, TokenPair{}, err
	}

	timezone := in.Timezone
	if timezone == "" {
		timezone = "UTC"
	}

	user, profile, err := s.users.Create(ctx, normalizeEmail(in.Email), hash, in.Name, timezone)
	if err != nil {
		if errors.Is(err, users.ErrEmailTaken) {
			return Account{}, TokenPair{}, ErrEmailTaken
		}
		return Account{}, TokenPair{}, err
	}

	pair, err := s.issuePair(ctx, user.ID, in.DeviceInfo)
	if err != nil {
		return Account{}, TokenPair{}, err
	}
	return Account{User: user, Profile: profile}, pair, nil
}

// Login verifies credentials and opens a session.
func (s *Service) Login(ctx context.Context, in LoginInput) (Account, TokenPair, error) {
	if err := ValidateLogin(in); err != nil {
		return Account{}, TokenPair{}, err
	}

	user, err := s.users.ByEmail(ctx, normalizeEmail(in.Email))
	if err != nil {
		if errors.Is(err, users.ErrNotFound) {
			// Hash anyway so an unknown email is not measurably faster than a
			// known one with a wrong password.
			_, _ = HashPassword(in.Password, s.hashParams)
			return Account{}, TokenPair{}, ErrInvalidCredentials
		}
		return Account{}, TokenPair{}, err
	}

	ok, err := VerifyPassword(in.Password, user.PasswordHash)
	if err != nil {
		return Account{}, TokenPair{}, fmt.Errorf("verify password: %w", err)
	}
	if !ok {
		return Account{}, TokenPair{}, ErrInvalidCredentials
	}

	profile, err := s.users.ProfileByUserID(ctx, user.ID)
	if err != nil && !errors.Is(err, users.ErrNotFound) {
		return Account{}, TokenPair{}, err
	}

	pair, err := s.issuePair(ctx, user.ID, in.DeviceInfo)
	if err != nil {
		return Account{}, TokenPair{}, err
	}
	return Account{User: user, Profile: profile}, pair, nil
}

// Refresh rotates the presented refresh token and mints a new access token.
// The old token stops working the instant this succeeds.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (TokenPair, error) {
	if err := validateRefreshToken(refreshToken); err != nil {
		return TokenPair{}, err
	}

	newToken, newHash, err := NewRefreshToken()
	if err != nil {
		return TokenPair{}, err
	}
	expiresAt := s.now().Add(s.refreshTTL)

	session, err := s.sessions.Rotate(ctx, HashRefreshToken(refreshToken), newHash, expiresAt)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return TokenPair{}, ErrInvalidToken
		}
		return TokenPair{}, err
	}

	access, accessExp, err := s.tokens.Issue(session.UserID)
	if err != nil {
		return TokenPair{}, err
	}
	return TokenPair{
		AccessToken:     access,
		AccessExpiresAt: accessExp,
		RefreshToken:    newToken,
		RefreshExpires:  session.ExpiresAt,
		SessionID:       session.ID,
	}, nil
}

// Logout deletes the session behind the presented refresh token.
func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	if err := validateRefreshToken(refreshToken); err != nil {
		return err
	}
	if err := s.sessions.DeleteByTokenHash(ctx, HashRefreshToken(refreshToken)); err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			// Logging out an already-dead session is not an error worth
			// surfacing; the client's intent is satisfied either way.
			return nil
		}
		return err
	}
	return nil
}

// Me loads the account behind a validated access token.
func (s *Service) Me(ctx context.Context, userID uuid.UUID) (Account, error) {
	user, err := s.users.ByID(ctx, userID)
	if err != nil {
		if errors.Is(err, users.ErrNotFound) {
			// Token is well-formed but the user is gone (deleted account).
			return Account{}, ErrInvalidToken
		}
		return Account{}, err
	}
	profile, err := s.users.ProfileByUserID(ctx, userID)
	if err != nil && !errors.Is(err, users.ErrNotFound) {
		return Account{}, err
	}
	return Account{User: user, Profile: profile}, nil
}

func (s *Service) issuePair(ctx context.Context, userID uuid.UUID, deviceInfo string) (TokenPair, error) {
	access, accessExp, err := s.tokens.Issue(userID)
	if err != nil {
		return TokenPair{}, err
	}
	refresh, refreshHash, err := NewRefreshToken()
	if err != nil {
		return TokenPair{}, err
	}

	var device *string
	if deviceInfo != "" {
		if len(deviceInfo) > maxDeviceInfoLen {
			deviceInfo = deviceInfo[:maxDeviceInfoLen]
		}
		device = &deviceInfo
	}

	expiresAt := s.now().Add(s.refreshTTL)
	session, err := s.sessions.Create(ctx, userID, refreshHash, expiresAt, device)
	if err != nil {
		return TokenPair{}, err
	}
	return TokenPair{
		AccessToken:     access,
		AccessExpiresAt: accessExp,
		RefreshToken:    refresh,
		RefreshExpires:  session.ExpiresAt,
		SessionID:       session.ID,
	}, nil
}
