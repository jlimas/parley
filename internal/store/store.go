// Package store persists parley posts, API keys, and agent identities.
// The broker keeps the in-memory hub as its read path; this package handles
// durability so a restart does not wipe the board.
//
// Audience is stored as a single JSON column to match the wire format and
// avoid a side table for what is conceptually one value.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "github.com/lib/pq"
	_ "modernc.org/sqlite"

	"github.com/jlimas/parley/internal/protocol"
)

// dialect abstracts driver-specific SQL differences. Add a new implementation
// when supporting a new backend (e.g. MySQL).
type dialect interface {
	// driverName is the database/sql driver name passed to sql.Open.
	driverName() string
	// rebind converts "?" placeholders to the driver-specific form.
	// SQLite and MySQL use "?"; PostgreSQL uses "$1", "$2", ...
	rebind(q string) string
	// columnExists reports whether table.column is present in the schema.
	columnExists(db *sql.DB, table, column string) (bool, error)
}

// sqliteDialect implements dialect for SQLite (modernc.org/sqlite driver).
type sqliteDialect struct{}

func (sqliteDialect) driverName() string     { return "sqlite" }
func (sqliteDialect) rebind(q string) string { return q }

func (sqliteDialect) columnExists(db *sql.DB, table, column string) (bool, error) {
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

// postgresDialect implements dialect for PostgreSQL (lib/pq driver).
type postgresDialect struct{}

func (postgresDialect) driverName() string { return "postgres" }

func (postgresDialect) rebind(q string) string {
	n := 0
	var out strings.Builder
	for i := 0; i < len(q); i++ {
		if q[i] == '?' {
			n++
			fmt.Fprintf(&out, "$%d", n)
		} else {
			out.WriteByte(q[i])
		}
	}
	return out.String()
}

func (postgresDialect) columnExists(db *sql.DB, table, column string) (bool, error) {
	var count int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM information_schema.columns WHERE table_name = $1 AND column_name = $2`,
		table, column,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("information_schema check %s.%s: %w", table, column, err)
	}
	return count > 0, nil
}

// detectDialect returns the dialect for the given DSN. DSNs starting with
// "postgres://" or "postgresql://" map to PostgreSQL; everything else is SQLite.
func detectDialect(dsn string) dialect {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return postgresDialect{}
	}
	return sqliteDialect{}
}

// Store is a thin wrapper over a SQL connection holding the parley tables.
type Store struct {
	db *sql.DB
	d  dialect
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

CREATE TABLE IF NOT EXISTS keys (
    id          TEXT PRIMARY KEY,
    description TEXT NOT NULL DEFAULT '',
    key_hash    TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    revoked_at  TEXT             -- NULL means active
);
CREATE INDEX IF NOT EXISTS keys_hash ON keys(key_hash);

CREATE TABLE IF NOT EXISTS agents (
    name       TEXT PRIMARY KEY,
    operator   TEXT NOT NULL DEFAULT '',
    last_seen  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS blobs (
    id           TEXT    PRIMARY KEY,
    content      TEXT    NOT NULL,
    content_type TEXT    NOT NULL DEFAULT '',
    filename     TEXT    NOT NULL DEFAULT '',
    size         INTEGER NOT NULL,
    created_at   TEXT    NOT NULL
);
`

// migrations are applied in order on every Open. Each entry is idempotent —
// it checks whether the change is already present before applying it.
// Append-only: never reorder, never delete.
var migrations = []func(*sql.DB, dialect) error{
	addTitleColumn,
	addBlobIDColumn,
}

func addTitleColumn(db *sql.DB, d dialect) error {
	has, err := d.columnExists(db, "posts", "title")
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

func addBlobIDColumn(db *sql.DB, d dialect) error {
	has, err := d.columnExists(db, "posts", "blob_id")
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE posts ADD COLUMN blob_id TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("add blob_id column: %w", err)
	}
	return nil
}

// Open opens the database at dsn. The backend (SQLite or PostgreSQL) is
// detected from the DSN prefix: "postgres://" or "postgresql://" selects
// PostgreSQL; anything else (file path or ":memory:") selects SQLite.
// The schema is created if missing and pending migrations are applied.
func Open(dsn string) (*Store, error) {
	d := detectDialect(dsn)
	db, err := sql.Open(d.driverName(), dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s %q: %w", d.driverName(), dsn, err)
	}
	// Serialize writes via the hub mutex; one connection avoids lock contention
	// on :memory: DSNs and is sufficient for the single-writer server model.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	for i, m := range migrations {
		if err := m(db, d); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("migration %d: %w", i, err)
		}
	}
	return &Store{db: db, d: d}, nil
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
	q := s.d.rebind(
		`INSERT INTO posts (id, parent_id, author, audience, title, content, blob_id, timestamp, seq)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM posts))`,
	)
	_, err = s.db.Exec(q,
		p.ID, p.ParentID, p.Author, string(aud), p.Title, p.Content, p.BlobID,
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
		`SELECT id, parent_id, author, audience, title, content, blob_id, timestamp
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
		if err := rows.Scan(&p.ID, &p.ParentID, &p.Author, &audRaw, &p.Title, &p.Content, &p.BlobID, &ts); err != nil {
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

// -- Blob storage --

// SaveBlob stores raw content and returns its generated ID. Content is stored
// base64-encoded so the same TEXT column works across SQLite and PostgreSQL.
func (s *Store) SaveBlob(contentType, filename string, content []byte) (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate blob id: %w", err)
	}
	id := hex.EncodeToString(raw[:])
	encoded := base64.StdEncoding.EncodeToString(content)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	q := s.d.rebind(
		`INSERT INTO blobs (id, content, content_type, filename, size, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
	)
	_, err := s.db.Exec(q, id, encoded, contentType, filename, int64(len(content)), now)
	if err != nil {
		return "", fmt.Errorf("insert blob: %w", err)
	}
	return id, nil
}

// LoadBlob retrieves the content and metadata for the blob with the given ID.
// Returns (nil, "", "", ErrBlobNotFound) when the ID is unknown.
func (s *Store) LoadBlob(id string) (content []byte, contentType, filename string, err error) {
	q := s.d.rebind(
		`SELECT content, content_type, filename FROM blobs WHERE id = ?`,
	)
	var encoded string
	if err := s.db.QueryRow(q, id).Scan(&encoded, &contentType, &filename); err != nil {
		if err == sql.ErrNoRows {
			return nil, "", "", ErrBlobNotFound
		}
		return nil, "", "", fmt.Errorf("load blob %s: %w", id, err)
	}
	content, err = base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, "", "", fmt.Errorf("decode blob %s: %w", id, err)
	}
	return content, contentType, filename, nil
}

// ErrBlobNotFound is returned by LoadBlob when no blob with that ID exists.
var ErrBlobNotFound = fmt.Errorf("blob not found")

// -- API key management --

// KeyRecord is the public view of a stored key. The plaintext and hash are
// never exposed here; the plaintext is printed once by CreateKey and then lost.
type KeyRecord struct {
	ID          string
	Description string
	CreatedAt   time.Time
	RevokedAt   time.Time // zero = active
}

// CreateKey mints a new opaque API key with the given description. Returns
// the plaintext key (shown exactly once to the caller) and the stored record.
// The key has the form prl_<64 hex chars> (32 random bytes encoded as hex).
func (s *Store) CreateKey(description string) (plaintext string, rec KeyRecord, err error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", KeyRecord{}, fmt.Errorf("generate key bytes: %w", err)
	}
	plaintext = "prl_" + hex.EncodeToString(raw[:])

	var idBytes [8]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return "", KeyRecord{}, fmt.Errorf("generate key id: %w", err)
	}
	id := hex.EncodeToString(idBytes[:])

	hash := sha256Hex(plaintext)
	now := time.Now().UTC()

	q := s.d.rebind(`INSERT INTO keys (id, description, key_hash, created_at) VALUES (?, ?, ?, ?)`)
	if _, err := s.db.Exec(q, id, description, hash, now.Format(time.RFC3339Nano)); err != nil {
		return "", KeyRecord{}, fmt.Errorf("insert key: %w", err)
	}

	return plaintext, KeyRecord{ID: id, Description: description, CreatedAt: now}, nil
}

// ListKeys returns all keys (active and revoked) ordered by creation time.
func (s *Store) ListKeys() ([]KeyRecord, error) {
	rows, err := s.db.Query(
		`SELECT id, description, created_at, revoked_at FROM keys ORDER BY created_at ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("query keys: %w", err)
	}
	defer rows.Close()

	var out []KeyRecord
	for rows.Next() {
		var (
			rec      KeyRecord
			created  string
			revokedS sql.NullString
		)
		if err := rows.Scan(&rec.ID, &rec.Description, &created, &revokedS); err != nil {
			return nil, fmt.Errorf("scan key: %w", err)
		}
		t, err := time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, fmt.Errorf("parse key created_at: %w", err)
		}
		rec.CreatedAt = t.UTC()
		if revokedS.Valid {
			t, err := time.Parse(time.RFC3339Nano, revokedS.String)
			if err != nil {
				return nil, fmt.Errorf("parse key revoked_at: %w", err)
			}
			rec.RevokedAt = t.UTC()
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// RevokeKey sets revoked_at on the key with the given ID. Returns false if
// the ID is not found or already revoked.
func (s *Store) RevokeKey(id string) (bool, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	q := s.d.rebind(`UPDATE keys SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`)
	res, err := s.db.Exec(q, now, id)
	if err != nil {
		return false, fmt.Errorf("revoke key: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ValidateKey reports whether the given raw API key is active (not revoked).
// The check uses a SHA-256 hash lookup with a database index, so it is fast
// enough to run on every request.
func (s *Store) ValidateKey(key string) bool {
	hash := sha256Hex(key)
	q := s.d.rebind(`SELECT COUNT(*) FROM keys WHERE key_hash = ? AND revoked_at IS NULL`)
	var count int
	err := s.db.QueryRow(q, hash).Scan(&count)
	return err == nil && count > 0
}

// DescriptionForKey returns the description (agent name) of the active key
// matching the given plaintext. Returns ("", false) if the key is unknown or
// revoked.
func (s *Store) DescriptionForKey(key string) (string, bool) {
	hash := sha256Hex(key)
	q := s.d.rebind(`SELECT description FROM keys WHERE key_hash = ? AND revoked_at IS NULL`)
	var desc string
	err := s.db.QueryRow(q, hash).Scan(&desc)
	if err != nil {
		return "", false
	}
	return desc, true
}

// -- Agent identity tracking --

// UpsertAgent records or updates the operator identity for a named agent.
// Called on each authenticated request so the mapping stays current.
func (s *Store) UpsertAgent(name, operator string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	q := s.d.rebind(
		`INSERT INTO agents (name, operator, last_seen) VALUES (?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET operator = excluded.operator, last_seen = excluded.last_seen`,
	)
	_, err := s.db.Exec(q, name, operator, now)
	if err != nil {
		return fmt.Errorf("upsert agent %s: %w", name, err)
	}
	return nil
}

// ClearPosts deletes all rows from the posts table and the agents table.
// API keys are not affected. Returns the number of posts deleted.
// Pass clearKeys=true to also wipe the keys table.
func (s *Store) Clear(clearKeys bool) (posts int, keys int, err error) {
	res, err := s.db.Exec(`DELETE FROM posts`)
	if err != nil {
		return 0, 0, fmt.Errorf("clear posts: %w", err)
	}
	n, _ := res.RowsAffected()
	posts = int(n)

	if _, err := s.db.Exec(`DELETE FROM agents`); err != nil {
		return posts, 0, fmt.Errorf("clear agents: %w", err)
	}

	if clearKeys {
		res, err := s.db.Exec(`DELETE FROM keys`)
		if err != nil {
			return posts, 0, fmt.Errorf("clear keys: %w", err)
		}
		n, _ := res.RowsAffected()
		keys = int(n)
	}
	return posts, keys, nil
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
