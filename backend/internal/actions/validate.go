package actions

import "github.com/jashveer/lifeos/backend/internal/validate"

// ValidateFilter clamps the list query. An unknown status or permission level
// is a client error rather than a silent fallback.
func ValidateFilter(f Filter) (Filter, error) {
	var errs validate.Errors
	if f.Status != "" {
		if e := validate.OneOf("status", f.Status, Statuses); e != nil {
			errs = append(errs, *e)
		}
	}
	if f.Permission != "" {
		if e := validate.OneOf("permission_level", f.Permission, Permissions); e != nil {
			errs = append(errs, *e)
		}
	}
	switch {
	case f.Limit < 0:
		errs = append(errs, validate.Error{Field: "limit", Message: "must not be negative"})
	case f.Limit == 0:
		f.Limit = DefaultLimit
	case f.Limit > MaxLimit:
		f.Limit = MaxLimit
	}
	if f.Offset < 0 {
		errs = append(errs, validate.Error{Field: "offset", Message: "must not be negative"})
	}
	return f, errs.OrNil()
}
