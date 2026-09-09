package auth

import (
	"fmt"
	"net/mail"
	"strings"
	"unicode/utf8"
)

const (
	minPasswordLength = 12
	// bcrypt-style truncation does not apply to argon2id, but an unbounded
	// password is a cheap way to make the hasher burn CPU.
	maxPasswordLength = 1024
	maxEmailLength    = 254
	maxNameLength     = 200
	maxTimezoneLength = 64
	maxDeviceInfoLen  = 512
)

// ValidationError describes a single rejected field. It maps onto the
// "fields" object in the 400 response body.
type ValidationError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

type ValidationErrors []ValidationError

func (v ValidationErrors) Error() string {
	parts := make([]string, 0, len(v))
	for _, e := range v {
		parts = append(parts, e.Field+": "+e.Message)
	}
	return strings.Join(parts, "; ")
}

func (v ValidationErrors) OrNil() error {
	if len(v) == 0 {
		return nil
	}
	return v
}

// normalizeEmail lowercases and trims; the unique index is on lower(email).
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func validateEmail(email string) *ValidationError {
	switch {
	case email == "":
		return &ValidationError{"email", "is required"}
	case len(email) > maxEmailLength:
		return &ValidationError{"email", fmt.Sprintf("must be at most %d characters", maxEmailLength)}
	}
	addr, err := mail.ParseAddress(email)
	// ParseAddress accepts `Name <a@b.c>`; only the bare address form is valid
	// here, and it must have a domain part.
	if err != nil || addr.Address != email || strings.Count(email, "@") != 1 ||
		strings.HasPrefix(email, "@") || strings.HasSuffix(email, "@") ||
		!strings.Contains(email[strings.Index(email, "@"):], ".") {
		return &ValidationError{"email", "must be a valid email address"}
	}
	return nil
}

func validatePassword(password string) *ValidationError {
	switch {
	case password == "":
		return &ValidationError{"password", "is required"}
	case utf8.RuneCountInString(password) < minPasswordLength:
		return &ValidationError{"password", fmt.Sprintf("must be at least %d characters", minPasswordLength)}
	case len(password) > maxPasswordLength:
		return &ValidationError{"password", fmt.Sprintf("must be at most %d bytes", maxPasswordLength)}
	}
	return nil
}

// ValidateRegister checks a registration payload and returns every problem at
// once rather than failing on the first.
func ValidateRegister(in RegisterInput) error {
	var errs ValidationErrors
	// Validate what will actually be stored, not the raw input: surrounding
	// whitespace and casing are normalized away first.
	if e := validateEmail(normalizeEmail(in.Email)); e != nil {
		errs = append(errs, *e)
	}
	if e := validatePassword(in.Password); e != nil {
		errs = append(errs, *e)
	}
	if utf8.RuneCountInString(in.Name) > maxNameLength {
		errs = append(errs, ValidationError{"name", fmt.Sprintf("must be at most %d characters", maxNameLength)})
	}
	if utf8.RuneCountInString(in.Timezone) > maxTimezoneLength {
		errs = append(errs, ValidationError{"timezone", fmt.Sprintf("must be at most %d characters", maxTimezoneLength)})
	}
	return errs.OrNil()
}

// ValidateLogin checks a login payload. It deliberately does not enforce the
// password length rule: an existing password predating a policy change must
// still be able to log in, and echoing the rule here leaks it to attackers.
func ValidateLogin(in LoginInput) error {
	var errs ValidationErrors
	// Validate what will actually be stored, not the raw input: surrounding
	// whitespace and casing are normalized away first.
	if e := validateEmail(normalizeEmail(in.Email)); e != nil {
		errs = append(errs, *e)
	}
	if in.Password == "" {
		errs = append(errs, ValidationError{"password", "is required"})
	} else if len(in.Password) > maxPasswordLength {
		errs = append(errs, ValidationError{"password", fmt.Sprintf("must be at most %d bytes", maxPasswordLength)})
	}
	return errs.OrNil()
}

func validateRefreshToken(token string) error {
	if token == "" {
		return ValidationErrors{{"refresh_token", "is required"}}
	}
	return nil
}
