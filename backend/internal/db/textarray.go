package db

import (
	"encoding/json"
	"fmt"
)

// TextArray adapts a Postgres text[] column to a Go []string.
//
// database/sql hands the driver's rendering of an array straight to Scan,
// which for the pgx driver is the Postgres literal form (`{a,"b c"}`). Parsing
// that by hand means re-implementing array quoting and escaping, so the reads
// select `array_to_json(col)` instead and this type unmarshals it. Writes need
// no adapter: pgx encodes a []string parameter as text[] on its own.
type TextArray []string

// Scan implements sql.Scanner over the JSON rendering of the column.
func (a *TextArray) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*a = nil
		return nil
	case []byte:
		return json.Unmarshal(v, (*[]string)(a))
	case string:
		return json.Unmarshal([]byte(v), (*[]string)(a))
	default:
		return fmt.Errorf("text array: cannot scan %T", src)
	}
}

// ArrayToJSON is the SELECT expression that pairs with TextArray. The column is
// NOT NULL in every Phase 2 table, but the coalesce keeps the type honest if a
// later nullable column reuses it.
func ArrayToJSON(column string) string {
	return `coalesce(array_to_json(` + column + `), '[]'::json) AS ` + column
}

// TextArrayParam keeps a nil slice out of the write path. database/sql encodes
// a nil []string as SQL NULL, and the Phase 2 array columns are NOT NULL: "no
// tags" is an empty array, not a missing value.
func TextArrayParam(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
