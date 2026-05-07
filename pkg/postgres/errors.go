package postgres

import "errors"

// ErrNotFound is returned by store-layer code when a query expected a row
// and got none. Callers should map this to HTTP 404 at the gateway boundary.
var ErrNotFound = errors.New("postgres: row not found")

// ErrConflict signals a unique-constraint violation surfaced as a typed
// error so the gateway maps it to HTTP 409.
var ErrConflict = errors.New("postgres: unique violation")

// ErrSerialization signals a serialization-failure that the caller should
// retry. pgx returns SQLSTATE 40001/40P01.
var ErrSerialization = errors.New("postgres: serialization failure")
