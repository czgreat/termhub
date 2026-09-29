package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withMigrations runs fn with a different migration list.
func withMigrations(t *testing.T, list []string, fn func()) {
	t.Helper()
	saved := migrations
	migrations = list
	defer func() { migrations = saved }()
	fn()
}

// docs/M8 验收 4: an existing database is copied aside before it is migrated,
// and a failed migration leaves both the copy and the original usable.
func TestMigrationBacksUpFirst(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "termhub.db")
	v1 := []string{`CREATE TABLE things (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`}

	withMigrations(t, v1, func() {
		db, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		db.Exec(`INSERT INTO things(name) VALUES ('kept')`)
		db.Close()
	})
	if b, _ := filepath.Glob(filepath.Join(dir, "backups", "*")); len(b) != 0 {
		t.Fatalf("a brand-new database needs no backup: %v", b)
	}

	// A good second migration: backup taken, data carried over.
	v2 := append(append([]string{}, v1...), `ALTER TABLE things ADD COLUMN note TEXT NOT NULL DEFAULT ''`)
	withMigrations(t, v2, func() {
		db, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var name, note string
		if err := db.QueryRow(`SELECT name, note FROM things`).Scan(&name, &note); err != nil || name != "kept" {
			t.Fatalf("data after migrating: %q %v", name, err)
		}
		db.Close()
	})
	backups, _ := filepath.Glob(filepath.Join(dir, "backups", "pre-migrate-v1-*.db"))
	if len(backups) != 1 {
		t.Fatalf("expected one backup of version 1, got %v", backups)
	}

	// A broken third migration: refuse to start, say where the backup is, change nothing.
	v3 := append(append([]string{}, v2...), `CREATE TABLE broken (; this is not SQL`)
	withMigrations(t, v3, func() {
		_, err := Open(path)
		if err == nil || !strings.Contains(err.Error(), "migration 3 failed") || !strings.Contains(err.Error(), "pre-migrate-v2-") {
			t.Fatalf("expected a refusal naming the backup, got %v", err)
		}
	})
	withMigrations(t, v2, func() { // the original still opens with the version it is at
		db, err := Open(path)
		if err != nil {
			t.Fatalf("the database was damaged by the failed migration: %v", err)
		}
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM things`).Scan(&n)
		db.Close()
		if n != 1 {
			t.Fatalf("rows after the failed migration: %d", n)
		}
	})
	v2backup, _ := filepath.Glob(filepath.Join(dir, "backups", "pre-migrate-v2-*.db"))
	if len(v2backup) != 1 {
		t.Fatalf("backup before the failed migration: %v", v2backup)
	}
	if fi, _ := os.Stat(v2backup[0]); fi == nil || fi.Size() == 0 {
		t.Fatal("the backup is empty")
	}

	// An older build must not run on a newer database.
	withMigrations(t, v1, func() {
		if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Fatalf("an older build opened a newer database: %v", err)
		}
	})
}

func TestRealSchemaOpens(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, table := range []string{"users", "recovery_codes", "login_sessions", "trusted_devices", "audit_log", "settings",
		"nodes", "node_tokens", "cli_profiles", "profile_bindings", "sessions"} {
		if _, err := db.Exec(`SELECT 1 FROM ` + table + ` LIMIT 1`); err != nil {
			t.Errorf("table %s: %v", table, err)
		}
	}
	var fk int
	db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk)
	if fk != 1 {
		t.Error("foreign keys must be enforced")
	}
}
