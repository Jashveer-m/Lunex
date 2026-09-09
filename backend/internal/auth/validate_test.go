package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateEmail(t *testing.T) {
	valid := []string{
		"ada@example.com",
		"ada.lovelace+lifeos@example.co.uk",
		"a@b.io",
		"ada_l@sub.domain.example.com",
	}
	for _, email := range valid {
		t.Run("valid/"+email, func(t *testing.T) {
			if e := validateEmail(email); e != nil {
				t.Fatalf("validateEmail(%q) = %v, want nil", email, e.Message)
			}
		})
	}

	invalid := map[string]string{
		"empty":          "",
		"no at":          "ada.example.com",
		"no domain":      "ada@",
		"no local part":  "@example.com",
		"no dot":         "ada@localhost",
		"two at signs":   "ada@@example.com",
		"display name":   "Ada <ada@example.com>",
		"spaces inside":  "ada lovelace@example.com",
		"trailing comma": "ada@example.com,",
		"too long":       strings.Repeat("a", 250) + "@example.com",
	}
	for name, email := range invalid {
		t.Run("invalid/"+name, func(t *testing.T) {
			if e := validateEmail(email); e == nil {
				t.Fatalf("validateEmail(%q) = nil, want an error", email)
			}
		})
	}
}

func TestValidatePasswordLength(t *testing.T) {
	if e := validatePassword(strings.Repeat("a", minPasswordLength)); e != nil {
		t.Fatalf("minimum-length password rejected: %v", e.Message)
	}
	if e := validatePassword(strings.Repeat("a", minPasswordLength-1)); e == nil {
		t.Fatal("password one character below the minimum was accepted")
	}
	if e := validatePassword(strings.Repeat("a", maxPasswordLength+1)); e == nil {
		t.Fatal("password above the maximum was accepted")
	}
	// Length is counted in runes, not bytes: 12 multi-byte characters pass.
	if e := validatePassword(strings.Repeat("é", minPasswordLength)); e != nil {
		t.Fatalf("multi-byte password rejected: %v", e.Message)
	}
}

func TestValidateRegisterReportsEveryProblem(t *testing.T) {
	err := ValidateRegister(RegisterInput{
		Email:    "bad",
		Password: "short",
		Name:     strings.Repeat("n", maxNameLength+1),
		Timezone: strings.Repeat("t", maxTimezoneLength+1),
	})
	var verrs ValidationErrors
	if !errors.As(err, &verrs) {
		t.Fatalf("err = %v, want ValidationErrors", err)
	}
	if len(verrs) != 4 {
		t.Fatalf("got %d field errors, want 4: %v", len(verrs), verrs)
	}
}

// Login must not enforce the current password policy: an account created
// before a policy change still has to be able to sign in.
func TestValidateLoginAcceptsShortExistingPassword(t *testing.T) {
	if err := ValidateLogin(LoginInput{Email: testEmail, Password: "old"}); err != nil {
		t.Fatalf("ValidateLogin rejected a short existing password: %v", err)
	}
	if err := ValidateLogin(LoginInput{Email: testEmail}); err == nil {
		t.Fatal("ValidateLogin accepted an empty password")
	}
}

func TestNormalizeEmail(t *testing.T) {
	if got := normalizeEmail("  Ada@Example.COM \t"); got != "ada@example.com" {
		t.Fatalf("normalizeEmail = %q", got)
	}
}
