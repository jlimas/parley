// Package store persists parley posts. The broker keeps the in-memory hub as
// its read path; this package handles durability so a restart does not wipe
// the board.
//
// Audience is stored as a single JSON column to match the wire format and
// avoid a side table for what is conceptually one value.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/yalochat/parley/internal/protocol"
)

// Store is a thin wrapper over a SQLite connection holding the posts table.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS posts (
    id         TEXT    PRIMARY KEY,
    parent_id  TEXT    NOT NULL DEFAULT '',
    author     TEXT    NOT NULL,
    audience   TEXT    NOT NULL,
    title      TEXT    NOT NULL DEFAULT '',
    content    TEXT    NOT NULL,
    timestamp  TEXT    NOT NULL,
    seq        INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS posts_seq ON posts(seq);
`

// migrations are applied in order on every Open. Each entry checks whether
// it has already been applied (via PRAGMA table_info or similar) so re-runs
// are safe. Append-only: never reorder, never delete.
var migrations = []func(*sql.DB) error{
	addTitleColumn,
}

func addTitleColumn(db *sql.DB) error {
	has, err := columnExists(db, "posts", "title")
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE posts ADD COLUMN title TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("add title column: %w", err)
	}
	return nil
}

func columnExists(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return false, fmt.Errorf("pragma table_info(%s): %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notnull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			return false, fmt.Errorf("scan table_info: %w", err)
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// Open opens the SQLite database at dsn. Pass ":memory:" for an in-memory
// store (no persistence, useful for tests). The schema is created if missing.
func Open(dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", dsn, err)
	}
	// modernc.org/sqlite is happy with multiple connections but we serialise
	// writes ourselves via the hub mutex; one connection is enough and avoids
	// "database is locked" surprises with :memory: DSNs.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	for i, m := range migrations {
		if err := m(db); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("migration %d: %w", i, err)
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// Save persists p. Caller is expected to have populated ID and Timestamp.
func (s *Store) Save(p protocol.Post) error {
	aud, err := json.Marshal(p.Audience)
	if err != nil {
		return fmt.Errorf("marshal audience: %w", err)
	}
	_, err = s.db.Exec(
		`INSERT INTO posts (id, parent_id, author, audience, title, content, timestamp, seq)
		 VALUES (?, ?, ?, ?, ?, ?, ?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM posts))`,
		p.ID, p.ParentID, p.Author, string(aud), p.Title, p.Content,
		p.Timestamp.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("insert post %s: %w", p.ID, err)
	}
	return nil
}

// Load returns every stored post in publish order (the order they were
// originally Save'd).
func (s *Store) Load() ([]protocol.Post, error) {
	rows, err := s.db.Query(
		`SELECT id, parent_id, author, audience, title, content, timestamp
		 FROM posts ORDER BY seq ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("query posts: %w", err)
	}
	defer rows.Close()

	var out []protocol.Post
	for rows.Next() {
		var (
			p      protocol.Post
			audRaw string
			ts     string
		)
		if err := rows.Scan(&p.ID, &p.ParentID, &p.Author, &audRaw, &p.Title, &p.Content, &ts); err != nil {
			return nil, fmt.Errorf("scan post: %w", err)
		}
		if err := json.Unmarshal([]byte(audRaw), &p.Audience); err != nil {
			return nil, fmt.Errorf("unmarshal audience for %s: %w", p.ID, err)
		}
		parsed, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return nil, fmt.Errorf("parse timestamp for %s: %w", p.ID, err)
		}
		p.Timestamp = parsed.UTC()
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate posts: %w", err)
	}
	return out, nil
}
