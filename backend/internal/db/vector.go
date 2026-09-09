package db

import (
	"database/sql/driver"
	"strconv"
)

// Vector adapts a Go []float32 to a pgvector `vector` column.
//
// pgx has no encoder for the extension's type -- it is not a core Postgres
// type -- so parameters are sent in the extension's own text form,
// `[1,2,3]`, and cast with `$n::vector` at the call site. Building the literal
// here rather than with fmt.Sprintf matters: a document of 800 chunks means 800
// of these, each 768 numbers wide.
//
// Embeddings are written but never read back: nothing in the API returns a
// vector, and the similarity is computed in SQL. So this is a Valuer with no
// matching Scanner, deliberately.
type Vector []float32

// Value implements driver.Valuer.
func (v Vector) Value() (driver.Value, error) {
	if v == nil {
		return nil, nil
	}
	// Two bytes of brackets, then at most ~15 per float plus a comma.
	buf := make([]byte, 0, 2+len(v)*16)
	buf = append(buf, '[')
	for i, f := range v {
		if i > 0 {
			buf = append(buf, ',')
		}
		// 32-bit precision: the value came from a float32 and round-trips
		// exactly, without printing the noise a float64 formatting would add.
		buf = strconv.AppendFloat(buf, float64(f), 'g', -1, 32)
	}
	buf = append(buf, ']')
	return string(buf), nil
}
