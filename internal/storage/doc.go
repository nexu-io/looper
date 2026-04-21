// Package storage holds database access, migrations, and repositories.
//
// Phase 1 of the Go port uses github.com/mattn/go-sqlite3, registered under
// DriverName, so the storage layer can preserve the established schema and
// runtime behavior before optimizing for CGO-free distribution.
package storage
