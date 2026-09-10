package db

import "strings"

// Contains is the ILIKE pattern that matches s anywhere in a column, with s
// taken literally.
//
// The escaping is the part that matters. A search for "50%" or "task_2" typed
// by a user -- or written by a model choosing a tool argument -- would
// otherwise be a pattern: `%` matching anything and `_` any one character. The
// statements that use this spell `ESCAPE '\'` explicitly rather than relying
// on backslash being Postgres's default, so the pairing is visible where it is
// used.
func Contains(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(s) + "%"
}
