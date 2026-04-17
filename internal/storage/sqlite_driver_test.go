package storage

import (
	"database/sql"
	"slices"
	"testing"
)

func TestDriverNameRegistersPhase1SQLiteDriver(t *testing.T) {
	if !slices.Contains(sql.Drivers(), DriverName) {
		t.Fatalf("sql.Drivers() = %v, want %q to be registered", sql.Drivers(), DriverName)
	}
}

func TestPhase1SQLiteDriverSupportsInMemoryDatabase(t *testing.T) {
	db, err := sql.Open(DriverName, ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("db.Close() error = %v", err)
		}
	})

	if err := db.Ping(); err != nil {
		t.Fatalf("db.Ping() error = %v", err)
	}

	if _, err := db.Exec(`CREATE TABLE parity_check (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`); err != nil {
		t.Fatalf("db.Exec(CREATE TABLE) error = %v", err)
	}

	if _, err := db.Exec(`INSERT INTO parity_check (name) VALUES (?)`, "phase-1"); err != nil {
		t.Fatalf("db.Exec(INSERT) error = %v", err)
	}

	var name string
	if err := db.QueryRow(`SELECT name FROM parity_check WHERE id = 1`).Scan(&name); err != nil {
		t.Fatalf("db.QueryRow().Scan() error = %v", err)
	}

	if name != "phase-1" {
		t.Fatalf("queried name = %q, want %q", name, "phase-1")
	}
}
