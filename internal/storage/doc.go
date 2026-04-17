// Package storage holds database access, migrations, and repositories.
//
// Phase 1 of the Go port uses github.com/mattn/go-sqlite3, registered under
// DriverName, so the storage rewrite can prioritize schema and
// runtime-behavior parity with the existing Bun/SQLite daemon before
// optimizing for CGO-free distribution.
package storage
