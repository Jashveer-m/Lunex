package auth

import (
	"context"
	"errors"
	"testing"
	"time"
)

const (
	testEmail    = "ada@example.com"
	testPassword = "correct horse battery staple"
)

func registerTestUser(t *testing.T, svc *Service) (Account, TokenPair) {
	t.Helper()
	account, pair, err := svc.Register(context.Background(), RegisterInput{
		Email:    testEmail,
		Password: testPassword,
		Name:     "Ada",
		Timezone: "Europe/London",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	return account, pair
}

func TestRegisterCreatesUserProfileAndSession(t *testing.T) {
	svc, userStore, sessions := newTestService(t)
	account, pair := registerTestUser(t, svc)

	if account.User.Email != testEmail {
		t.Fatalf("email = %q, want %q", account.User.Email, testEmail)
	}
	if account.Profile.UserID != account.User.ID {
		t.Fatal("profile is not linked to the created user")
	}
	if account.Profile.Name != "Ada" || account.Profile.Timezone != "Europe/London" {
		t.Fatalf("profile = %+v, want name Ada / tz Europe/London", account.Profile)
	}
	if sessions.count() != 1 {
		t.Fatalf("session count = %d, want 1", sessions.count())
	}

	stored := userStore.byID[account.User.ID]
	if stored.PasswordHash == testPassword {
		t.Fatal("password was stored in plaintext")
	}
	ok, err := VerifyPassword(testPassword, stored.PasswordHash)
	if err != nil || !ok {
		t.Fatalf("stored hash does not verify the password: ok=%v err=%v", ok, err)
	}

	if _, err := svc.tokens.Parse(pair.AccessToken); err != nil {
		t.Fatalf("issued access token does not parse: %v", err)
	}
	if pair.RefreshToken == "" {
		t.Fatal("no refresh token issued")
	}
	if _, found := sessions.byHash[HashRefreshToken(pair.RefreshToken)]; !found {
		t.Fatal("refresh token is not stored as a hash")
	}
	for hash := range sessions.byHash {
		if hash == pair.RefreshToken {
			t.Fatal("refresh token was stored verbatim instead of hashed")
		}
	}
}

func TestRegisterNormalizesEmailAndDefaultsTimezone(t *testing.T) {
	svc, _, _ := newTestService(t)
	account, _, err := svc.Register(context.Background(), RegisterInput{
		Email:    "  Ada@Example.COM ",
		Password: testPassword,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if account.User.Email != testEmail {
		t.Fatalf("email = %q, want it normalized to %q", account.User.Email, testEmail)
	}
	if account.Profile.Timezone != "UTC" {
		t.Fatalf("timezone = %q, want the UTC default", account.Profile.Timezone)
	}
}

func TestRegisterRejectsDuplicateEmail(t *testing.T) {
	svc, _, _ := newTestService(t)
	registerTestUser(t, svc)

	_, _, err := svc.Register(context.Background(), RegisterInput{
		Email:    "ADA@example.com", // same address, different case
		Password: "another-password-9876",
	})
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("err = %v, want ErrEmailTaken", err)
	}
}

func TestRegisterValidation(t *testing.T) {
	svc, _, sessions := newTestService(t)
	tests := []struct {
		name  string
		in    RegisterInput
		field string
	}{
		{"missing email", RegisterInput{Password: testPassword}, "email"},
		{"malformed email", RegisterInput{Email: "not-an-email", Password: testPassword}, "email"},
		{"email without domain dot", RegisterInput{Email: "ada@localhost", Password: testPassword}, "email"},
		{"display-name form", RegisterInput{Email: "Ada <ada@example.com>", Password: testPassword}, "email"},
		{"missing password", RegisterInput{Email: testEmail}, "password"},
		{"short password", RegisterInput{Email: testEmail, Password: "short1234"}, "password"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := svc.Register(context.Background(), tc.in)
			var verrs ValidationErrors
			if !errors.As(err, &verrs) {
				t.Fatalf("err = %v, want ValidationErrors", err)
			}
			var found bool
			for _, e := range verrs {
				if e.Field == tc.field {
					found = true
				}
			}
			if !found {
				t.Fatalf("no validation error on %q, got %v", tc.field, verrs)
			}
		})
	}
	if sessions.count() != 0 {
		t.Fatal("an invalid registration created a session")
	}
}

func TestLoginSucceedsWithCorrectPassword(t *testing.T) {
	svc, _, sessions := newTestService(t)
	registered, _ := registerTestUser(t, svc)

	account, pair, err := svc.Login(context.Background(), LoginInput{
		Email:      "ADA@example.com",
		Password:   testPassword,
		DeviceInfo: "curl/8.4",
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if account.User.ID != registered.User.ID {
		t.Fatal("login returned a different user")
	}
	if account.Profile.Name != "Ada" {
		t.Fatalf("profile name = %q, want Ada", account.Profile.Name)
	}
	if sessions.count() != 2 {
		t.Fatalf("session count = %d, want 2 (register + login)", sessions.count())
	}

	session := sessions.byHash[HashRefreshToken(pair.RefreshToken)]
	if session.DeviceInfo == nil || *session.DeviceInfo != "curl/8.4" {
		t.Fatalf("device info = %v, want curl/8.4", session.DeviceInfo)
	}
}

func TestLoginRejectsWrongPasswordAndUnknownUser(t *testing.T) {
	svc, _, sessions := newTestService(t)
	registerTestUser(t, svc)

	tests := []struct {
		name string
		in   LoginInput
	}{
		{"wrong password", LoginInput{Email: testEmail, Password: "wrong-password-12345"}},
		{"unknown email", LoginInput{Email: "nobody@example.com", Password: testPassword}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := svc.Login(context.Background(), tc.in)
			if !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("err = %v, want ErrInvalidCredentials", err)
			}
		})
	}
	if sessions.count() != 1 {
		t.Fatalf("failed logins created sessions: count = %d, want 1", sessions.count())
	}
}

// An unknown email and a wrong password must be indistinguishable to the
// caller — same error value, and no early return that skips the hash work.
func TestLoginDoesNotLeakUserExistence(t *testing.T) {
	svc, _, _ := newTestService(t)
	registerTestUser(t, svc)

	_, _, wrongPass := svc.Login(context.Background(), LoginInput{Email: testEmail, Password: "wrong-password-12345"})
	_, _, unknown := svc.Login(context.Background(), LoginInput{Email: "nobody@example.com", Password: "wrong-password-12345"})
	if wrongPass.Error() != unknown.Error() {
		t.Fatalf("distinguishable errors: %q vs %q", wrongPass, unknown)
	}
}

func TestRefreshRotatesToken(t *testing.T) {
	svc, _, sessions := newTestService(t)
	account, first := registerTestUser(t, svc)

	second, err := svc.Refresh(context.Background(), first.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if second.RefreshToken == first.RefreshToken {
		t.Fatal("refresh token was not rotated")
	}
	if second.SessionID != first.SessionID {
		t.Fatal("rotation should reuse the same session row")
	}
	if sessions.count() != 1 {
		t.Fatalf("session count = %d, want 1 after rotation", sessions.count())
	}
	if _, stale := sessions.byHash[HashRefreshToken(first.RefreshToken)]; stale {
		t.Fatal("the old refresh token hash is still stored")
	}

	userID, err := svc.tokens.Parse(second.AccessToken)
	if err != nil {
		t.Fatalf("new access token does not parse: %v", err)
	}
	if userID != account.User.ID {
		t.Fatal("new access token belongs to a different user")
	}
}

func TestRefreshRejectsReusedToken(t *testing.T) {
	svc, _, _ := newTestService(t)
	_, first := registerTestUser(t, svc)

	if _, err := svc.Refresh(context.Background(), first.RefreshToken); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}
	if _, err := svc.Refresh(context.Background(), first.RefreshToken); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("replayed refresh token: err = %v, want ErrInvalidToken", err)
	}
}

func TestRefreshRejectsUnknownAndEmptyTokens(t *testing.T) {
	svc, _, _ := newTestService(t)
	registerTestUser(t, svc)

	if _, err := svc.Refresh(context.Background(), "not-a-real-token"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("err = %v, want ErrInvalidToken", err)
	}
	var verrs ValidationErrors
	if _, err := svc.Refresh(context.Background(), ""); !errors.As(err, &verrs) {
		t.Fatalf("err = %v, want ValidationErrors for an empty token", err)
	}
}

func TestRefreshRejectsExpiredSession(t *testing.T) {
	svc, _, sessions := newTestService(t)
	_, pair := registerTestUser(t, svc)

	// Advance the store's clock past the refresh TTL.
	sessions.now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	if _, err := svc.Refresh(context.Background(), pair.RefreshToken); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired session: err = %v, want ErrInvalidToken", err)
	}
}

func TestLogoutInvalidatesOnlyItsOwnSession(t *testing.T) {
	svc, _, sessions := newTestService(t)
	_, fromRegister := registerTestUser(t, svc)
	_, fromLogin, err := svc.Login(context.Background(), LoginInput{Email: testEmail, Password: testPassword})
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.Logout(context.Background(), fromLogin.RefreshToken); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if sessions.count() != 1 {
		t.Fatalf("session count = %d, want 1 after logging out one of two", sessions.count())
	}
	if _, err := svc.Refresh(context.Background(), fromLogin.RefreshToken); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("the logged-out refresh token still works")
	}
	if _, err := svc.Refresh(context.Background(), fromRegister.RefreshToken); err != nil {
		t.Fatalf("the other session was invalidated too: %v", err)
	}
}

// Logging out twice is not an error: the client's intent is already satisfied.
func TestLogoutIsIdempotent(t *testing.T) {
	svc, _, _ := newTestService(t)
	_, pair := registerTestUser(t, svc)

	for i := range 2 {
		if err := svc.Logout(context.Background(), pair.RefreshToken); err != nil {
			t.Fatalf("Logout call %d: %v", i+1, err)
		}
	}
}

func TestMeReturnsAccount(t *testing.T) {
	svc, _, _ := newTestService(t)
	registered, _ := registerTestUser(t, svc)

	account, err := svc.Me(context.Background(), registered.User.ID)
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if account.User.ID != registered.User.ID || account.Profile.Name != "Ada" {
		t.Fatalf("Me returned %+v", account)
	}
}

func TestMeRejectsDeletedUser(t *testing.T) {
	svc, userStore, _ := newTestService(t)
	registered, _ := registerTestUser(t, svc)
	delete(userStore.byID, registered.User.ID)

	if _, err := svc.Me(context.Background(), registered.User.ID); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("err = %v, want ErrInvalidToken for a deleted user", err)
	}
}

func TestServicePropagatesStoreFailures(t *testing.T) {
	svc, userStore, _ := newTestService(t)
	boom := errors.New("database is on fire")
	userStore.failWith = boom

	if _, _, err := svc.Register(context.Background(), RegisterInput{Email: testEmail, Password: testPassword}); !errors.Is(err, boom) {
		t.Fatalf("Register err = %v, want the store error", err)
	}
	if _, _, err := svc.Login(context.Background(), LoginInput{Email: testEmail, Password: testPassword}); !errors.Is(err, boom) {
		t.Fatalf("Login err = %v, want the store error", err)
	}
}
