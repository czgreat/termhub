// Package store opens the Hub's SQLite database and applies migrations
// (docs/M8 第 2 节). It knows table layouts, not what they mean.
package store

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// migrations are applied in order and never edited once released: a change
// to the schema is a new entry.
var migrations = []string{
	// 1: accounts and authentication (docs/M6 第 2 节)
	`CREATE TABLE users (
		id INTEGER PRIMARY KEY,
		username TEXT NOT NULL COLLATE NOCASE UNIQUE,
		display_name TEXT NOT NULL DEFAULT '',
		role TEXT NOT NULL CHECK (role IN ('admin','user')),
		status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
		password_hash TEXT NOT NULL,
		must_change_password INTEGER NOT NULL DEFAULT 0,
		totp_secret BLOB,
		totp_confirmed_at INTEGER,
		totp_last_step INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL,
		last_login_at INTEGER
	);
	CREATE TABLE recovery_codes (
		user_id INTEGER NOT NULL REFERENCES users(id),
		code_hash BLOB NOT NULL,
		used_at INTEGER,
		PRIMARY KEY (user_id, code_hash)
	);
	CREATE TABLE login_sessions (
		token_hash BLOB PRIMARY KEY,
		user_id INTEGER NOT NULL REFERENCES users(id),
		created_at INTEGER NOT NULL,
		last_seen_at INTEGER NOT NULL,
		ip TEXT NOT NULL DEFAULT '',
		user_agent TEXT NOT NULL DEFAULT '',
		csrf_token TEXT NOT NULL,
		reverified_until INTEGER NOT NULL DEFAULT 0,
		device_hash BLOB
	);
	CREATE INDEX login_sessions_user ON login_sessions(user_id);
	CREATE TABLE trusted_devices (
		token_hash BLOB PRIMARY KEY,
		user_id INTEGER NOT NULL REFERENCES users(id),
		name TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		last_used_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL
	);
	CREATE INDEX trusted_devices_user ON trusted_devices(user_id);
	CREATE TABLE audit_log (
		id INTEGER PRIMARY KEY,
		at INTEGER NOT NULL,
		user_id INTEGER,
		username TEXT NOT NULL DEFAULT '',
		ip TEXT NOT NULL DEFAULT '',
		event TEXT NOT NULL,
		object TEXT NOT NULL DEFAULT '',
		result TEXT NOT NULL DEFAULT 'ok',
		detail TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX audit_log_at ON audit_log(at);
	CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);`,

	// 2: nodes, CLI profiles, bindings, session register (docs/M7 第 2 节)
	`CREATE TABLE nodes (
		id INTEGER PRIMARY KEY,
		name TEXT NOT NULL COLLATE NOCASE UNIQUE,
		note TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'enabled' CHECK (status IN ('enabled','disabled')),
		fingerprint TEXT NOT NULL DEFAULT '',
		fingerprint_mismatch INTEGER NOT NULL DEFAULT 0,
		last_seen_at INTEGER,
		agent_ver TEXT NOT NULL DEFAULT '',
		host_ver TEXT NOT NULL DEFAULT '',
		os_build TEXT NOT NULL DEFAULT '',
		pwsh_store INTEGER NOT NULL DEFAULT 0,
		canary INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL
	);
	CREATE TABLE node_tokens (
		token_hash BLOB PRIMARY KEY,
		node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
		created_at INTEGER NOT NULL,
		last_used_at INTEGER,
		expires_at INTEGER
	);
	CREATE TABLE cli_profiles (
		id INTEGER PRIMARY KEY,
		node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
		name TEXT NOT NULL,
		kind TEXT NOT NULL DEFAULT 'custom',
		mode TEXT NOT NULL CHECK (mode IN ('shell','direct')),
		shell_path TEXT NOT NULL DEFAULT '',
		shell_args TEXT NOT NULL DEFAULT '[]',
		command TEXT NOT NULL DEFAULT '',
		args TEXT NOT NULL DEFAULT '[]',
		resume_cmd TEXT NOT NULL DEFAULT '',
		env BLOB,
		default_cwd TEXT NOT NULL DEFAULT '',
		idle_timeout INTEGER NOT NULL DEFAULT 0,
		quote_style TEXT NOT NULL DEFAULT 'auto',
		status TEXT NOT NULL DEFAULT 'enabled' CHECK (status IN ('enabled','disabled')),
		created_at INTEGER NOT NULL
	);
	CREATE TABLE profile_bindings (
		profile_id INTEGER NOT NULL REFERENCES cli_profiles(id) ON DELETE CASCADE,
		user_id INTEGER NOT NULL REFERENCES users(id),
		PRIMARY KEY (profile_id, user_id)
	);
	CREATE TABLE sessions (
		sid TEXT PRIMARY KEY,
		node_id INTEGER NOT NULL,
		profile_id INTEGER,
		profile_name TEXT NOT NULL DEFAULT '',
		owner_id INTEGER,
		cwd TEXT NOT NULL DEFAULT '',
		title TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		ended_at INTEGER,
		end_reason TEXT NOT NULL DEFAULT '',
		exit_code INTEGER
	);
	CREATE INDEX sessions_owner ON sessions(owner_id, ended_at);
	CREATE INDEX sessions_node ON sessions(node_id, ended_at);`,
	// 3: second-factor lockout counters (docs/M6 第 7 节, 2026-09-23 安全复核 C1)
	`ALTER TABLE users ADD COLUMN mfa_failures INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE users ADD COLUMN mfa_window_start INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE users ADD COLUMN mfa_locked_until INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE users ADD COLUMN mfa_lockouts INTEGER NOT NULL DEFAULT 0;`,
	// 4: long-term second-factor failure counter (安全复核第二轮)
	`ALTER TABLE users ADD COLUMN mfa_total INTEGER NOT NULL DEFAULT 0;`,
	// 5: display order of nodes (the administrator's choice; ties by name)
	`ALTER TABLE nodes ADD COLUMN position INTEGER NOT NULL DEFAULT 0;`,
	// 6: history (docs/M10 第 3 节): the "continue the last one" command of a
	// profile, the conversation a session reopened, and the conversations a
	// user hid from the list (the CLI's own files are never touched)
	`ALTER TABLE cli_profiles ADD COLUMN continue_cmd TEXT NOT NULL DEFAULT '';
	UPDATE cli_profiles SET continue_cmd='claude --continue' WHERE kind='claude';
	UPDATE cli_profiles SET continue_cmd='codex resume --last' WHERE kind='codex';
	ALTER TABLE sessions ADD COLUMN conv_id TEXT NOT NULL DEFAULT '';
	CREATE TABLE history_hidden (
		user_id INTEGER NOT NULL,
		profile_id INTEGER NOT NULL,
		conv_id TEXT NOT NULL,
		hidden_at INTEGER NOT NULL,
		PRIMARY KEY (user_id, profile_id, conv_id)
	);`,
	// 7: the folders whose history a bound user sees, per binding (docs/M10
	// 第 3.4 节); a JSON array, empty = all. Not an isolation: a view.
	`ALTER TABLE profile_bindings ADD COLUMN folders TEXT NOT NULL DEFAULT '[]';`,
	// 8: a user's own view of history folders (docs/M10 第 3.5 节): hidden,
	// or shown under another name. The folders themselves are never touched.
	`CREATE TABLE history_folders (
		user_id INTEGER NOT NULL,
		profile_id INTEGER NOT NULL,
		folder TEXT NOT NULL,
		hidden INTEGER NOT NULL DEFAULT 0,
		name TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (user_id, profile_id, folder)
	);`,
	// 9: the kind of CLI a session was started as, kept with the session: one
	// AI CLI of a kind per folder holds though its profile is later deleted or
	// changed (Codex 第五轮 2). Running sessions take their profile's kind now.
	`ALTER TABLE sessions ADD COLUMN kind TEXT NOT NULL DEFAULT '';
	UPDATE sessions SET kind=COALESCE((SELECT kind FROM cli_profiles c WHERE c.id=sessions.profile_id),'') WHERE ended_at IS NULL;`,
}

// DB is the opened database.
type DB struct {
	*sql.DB
	Path string
}

// Open opens (creating if needed) the database at path and migrates it. When
// migrations are pending on an existing database, the file is copied aside
// first; a failed migration leaves that copy untouched and returns an error
// naming it.
func Open(path string) (*DB, error) {
	_, statErr := os.Stat(path)
	existed := statErr == nil
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer; reads are short. Revisit with a read pool if it ever matters.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		db.Close()
		return nil, err
	}
	var current int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&current); err != nil {
		db.Close()
		return nil, err
	}
	if current > len(migrations) {
		db.Close()
		return nil, fmt.Errorf("store: database is at version %d, this build knows %d; refusing to run an older build on it", current, len(migrations))
	}
	if current < len(migrations) {
		backup := ""
		if existed && current > 0 {
			db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
			backup = filepath.Join(filepath.Dir(path), "backups",
				fmt.Sprintf("pre-migrate-v%d-%s.db", current, time.Now().Format("20060102-150405")))
			if err := copyFile(path, backup); err != nil {
				db.Close()
				return nil, fmt.Errorf("store: cannot back up before migrating: %w", err)
			}
		}
		for v := current + 1; v <= len(migrations); v++ {
			tx, err := db.Begin()
			if err == nil {
				if _, err = tx.Exec(migrations[v-1]); err == nil {
					_, err = tx.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES (?,?)`, v, time.Now().Unix())
				}
				if err == nil {
					err = tx.Commit()
				} else {
					tx.Rollback()
				}
			}
			if err != nil {
				db.Close()
				return nil, fmt.Errorf("store: migration %d failed (backup: %q): %w", v, backup, err)
			}
		}
	}
	return &DB{DB: db, Path: path}, nil
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
