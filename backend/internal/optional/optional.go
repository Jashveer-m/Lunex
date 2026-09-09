// Package optional provides the three-state field a PATCH body needs.
//
// A plain *T collapses "the client omitted this key" and "the client sent
// null", which makes a nullable column impossible to clear over the wire: the
// handler cannot tell "leave the deadline alone" from "remove the deadline".
// Field keeps the two apart.
package optional

import "encoding/json"

type Field[T any] struct {
	Set   bool // the key was present in the request body
	Value *T   // nil when the key was present and explicitly null
}

// UnmarshalJSON is only ever called when the key is present in the object being
// decoded, which is precisely what makes Set meaningful.
func (f *Field[T]) UnmarshalJSON(data []byte) error {
	f.Set = true
	if string(data) == "null" {
		f.Value = nil
		return nil
	}
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	f.Value = &v
	return nil
}

// Get returns the value and whether one was supplied — present and not null.
func (f Field[T]) Get() (T, bool) {
	var zero T
	if !f.Set || f.Value == nil {
		return zero, false
	}
	return *f.Value, true
}

// Cleared reports whether the client explicitly asked for the field to be NULL.
func (f Field[T]) Cleared() bool { return f.Set && f.Value == nil }

// Of builds a Field holding a value; it exists for tests and callers that
// construct service inputs directly rather than decoding them.
func Of[T any](v T) Field[T] { return Field[T]{Set: true, Value: &v} }

// Null builds a Field that clears the column.
func Null[T any]() Field[T] { return Field[T]{Set: true} }
