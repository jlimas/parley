// Package store persists parley posts, API keys, tenants, and agent identities.
// The broker keeps the in-memory hub as its read path; this package handles
// durability so a restart does not wipe the board.
//
// Every resource (post, key, agent, blob) belongs to a tenant. Queries are
// always scoped to a tenant_id so boards are fully isolated.
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

// dialect abstracts driver-specific SQL differences.
type dialect interface {
	driverName() string
	rebind(q string) string
}

type sqliteDialect struct{}

func (sqliteDialect) driverName() string     { return "sqlite" }
func (sqliteDialect) rebind(q string) string { return q }

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
CREATE TABLE IF NOT EXISTS tenants (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS keys (
    id          TEXT PRIMARY KEY,
    tenant_id   TEXT NOT NULL REFERENCES tenants(id),
    agent       TEXT NOT NULL,
    key_hash    TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    revoked_at  TEXT
);
CREATE INDEX IF NOT EXISTS keys_hash ON keys(key_hash);

CREATE TABLE IF NOT EXISTS posts (
    id         TEXT    PRIMARY KEY,
    tenant_id  TEXT    NOT NULL,
    parent_id  TEXT    NOT NULL DEFAULT '',
    author     TEXT    NOT NULL,
    audience   TEXT    NOT NULL,
    title      TEXT    NOT NULL DEFAULT '',
    content    TEXT    NOT NULL DEFAULT '',
    blob_id    TEXT    NOT NULL DEFAULT '',
    timestamp  TEXT    NOT NULL,
    seq        INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS posts_seq ON posts(seq);
CREATE INDEX IF NOT EXISTS posts_tenant ON posts(tenant_id);

CREATE TABLE IF NOT EXISTS agents (
    tenant_id TEXT NOT NULL,
    name      TEXT NOT NULL,
    operator  TEXT NOT NULL DEFAULT '',
    last_seen TEXT NOT NULL,
    PRIMARY KEY (tenant_id, name)
);

CREATE TABLE IF NOT EXISTS blobs (
    id           TEXT    PRIMARY KEY,
    tenant_id    TEXT    NOT NULL,
    content      TEXT    NOT NULL,
    content_type TEXT    NOT NULL DEFAULT '',
    filename     TEXT    NOT NULL DEFAULT '',
    size         INTEGER NOT NULL,
    created_at   TEXT    NOT NULL
);
`

// Open opens the database at dsn, creates the schema if missing, and returns
// a Store ready for use. No migrations are applied — schema is the source of
// truth; breaking changes require a fresh database.
func Open(dsn string) (*Store, error) {
	d := detectDialect(dsn)
	db, err := sql.Open(d.driverName(), dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s %q: %w", d.driverName(), dsn, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return &Store{db: db, d: d}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// -- Tenant management --

// TenantRecord is the public view of a stored tenant.
type TenantRecord struct {
	ID        string
	Name      string
	CreatedAt time.Time
}

// CreateTenant creates a new tenant with the given display name.
func (s *Store) CreateTenant(name string) (TenantRecord, error) {
	var idBytes [8]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return TenantRecord{}, fmt.Errorf("generate tenant id: %w", err)
	}
	id := hex.EncodeToString(idBytes[:])
	now := time.Now().UTC()
	q := s.d.rebind(`INSERT INTO tenants (id, name, created_at) VALUES (?, ?, ?)`)
	if _, err := s.db.Exec(q, id, name, now.Format(time.RFC3339Nano)); err != nil {
		return TenantRecord{}, fmt.Errorf("insert tenant: %w", err)
	}
	return TenantRecord{ID: id, Name: name, CreatedAt: now}, nil
}

// ListTenants returns all tenants ordered by creation time.
func (s *Store) ListTenants() ([]TenantRecord, error) {
	rows, err := s.db.Query(`SELECT id, name, created_at FROM tenants ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("query tenants: %w", err)
	}
	defer rows.Close()
	var out []TenantRecord
	for rows.Next() {
		var rec TenantRecord
		var created string
		if err := rows.Scan(&rec.ID, &rec.Name, &created); err != nil {
			return nil, fmt.Errorf("scan tenant: %w", err)
		}
		t, err := time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, fmt.Errorf("parse tenant created_at: %w", err)
		}
		rec.CreatedAt = t.UTC()
		out = append(out, rec)
	}
	return out, rows.Err()
}

// TenantExists reports whether a tenant with the given ID exists.
func (s *Store) TenantExists(id string) (bool, error) {
	var count int
	q := s.d.rebind(`SELECT COUNT(*) FROM tenants WHERE id = ?`)
	err := s.db.QueryRow(q, id).Scan(&count)
	return count > 0, err
}

// -- API key management --

// KeyRecord is the public view of a stored key.
type KeyRecord struct {
	ID        string
	TenantID  string
	Agent     string
	CreatedAt time.Time
	RevokedAt time.Time // zero = active
}

// CreateKey mints a new API key for the given tenant and agent name. The
// plaintext key is printed once and never stored.
func (s *Store) CreateKey(tenantID, agent string) (plaintext string, rec KeyRecord, err error) {
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

	q := s.d.rebind(`INSERT INTO keys (id, tenant_id, agent, key_hash, created_at) VALUES (?, ?, ?, ?, ?)`)
	if _, err := s.db.Exec(q, id, tenantID, agent, hash, now.Format(time.RFC3339Nano)); err != nil {
		return "", KeyRecord{}, fmt.Errorf("insert key: %w", err)
	}
	return plaintext, KeyRecord{ID: id, TenantID: tenantID, Agent: agent, CreatedAt: now}, nil
}

// ListKeys returns all keys for the given tenant ordered by creation time.
func (s *Store) ListKeys(tenantID string) ([]KeyRecord, error) {
	q := s.d.rebind(`SELECT id, tenant_id, agent, created_at, revoked_at FROM keys WHERE tenant_id = ? ORDER BY created_at ASC`)
	rows, err := s.db.Query(q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("query keys: %w", err)
	}
	defer rows.Close()
	return scanKeys(rows)
}

// ListAllKeys returns all keys across all tenants.
func (s *Store) ListAllKeys() ([]KeyRecord, error) {
	rows, err := s.db.Query(`SELECT id, tenant_id, agent, created_at, revoked_at FROM keys ORDER BY tenant_id, created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("query all keys: %w", err)
	}
	defer rows.Close()
	return scanKeys(rows)
}

func scanKeys(rows *sql.Rows) ([]KeyRecord, error) {
	var out []KeyRecord
	for rows.Next() {
		var rec KeyRecord
		var created string
		var revokedS sql.NullString
		if err := rows.Scan(&rec.ID, &rec.TenantID, &rec.Agent, &created, &revokedS); err != nil {
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
// not found or already revoked.
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
func (s *Store) ValidateKey(key string) bool {
	hash := sha256Hex(key)
	q := s.d.rebind(`SELECT COUNT(*) FROM keys WHERE key_hash = ? AND revoked_at IS NULL`)
	var count int
	err := s.db.QueryRow(q, hash).Scan(&count)
	return err == nil && count > 0
}

// AgentForKey resolves a raw API key to its (tenantID, agent) pair. Returns
// ("", "", false) if the key is unknown or revoked.
func (s *Store) AgentForKey(key string) (tenantID, agent string, ok bool) {
	hash := sha256Hex(key)
	q := s.d.rebind(`SELECT tenant_id, agent FROM keys WHERE key_hash = ? AND revoked_at IS NULL`)
	err := s.db.QueryRow(q, hash).Scan(&tenantID, &agent)
	if err != nil {
		return "", "", false
	}
	return tenantID, agent, true
}

// -- Post persistence --

// Save persists p. p.TenantID, p.ID, and p.Timestamp must be populated.
func (s *Store) Save(p protocol.Post) error {
	aud, err := json.Marshal(p.Audience)
	if err != nil {
		return fmt.Errorf("marshal audience: %w", err)
	}
	q := s.d.rebind(
		`INSERT INTO posts (id, tenant_id, parent_id, author, audience, title, content, blob_id, timestamp, seq)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?,
		         (SELECT COALESCE(MAX(seq), 0) + 1 FROM posts WHERE tenant_id = ?))`,
	)
	_, err = s.db.Exec(q,
		p.ID, p.TenantID, p.ParentID, p.Author, string(aud),
		p.Title, p.Content, p.BlobID,
		p.Timestamp.UTC().Format(time.RFC3339Nano),
		p.TenantID,
	)
	if err != nil {
		return fmt.Errorf("insert post %s: %w", p.ID, err)
	}
	return nil
}

// LoadByTenant returns every stored post grouped by tenant_id, each tenant's
// slice ordered by seq (publish order).
func (s *Store) LoadByTenant() (map[string][]protocol.Post, error) {
	rows, err := s.db.Query(
		`SELECT id, tenant_id, parent_id, author, audience, title, content, blob_id, timestamp
		 FROM posts ORDER BY tenant_id, seq ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("query posts: %w", err)
	}
	defer rows.Close()

	out := make(map[string][]protocol.Post)
	for rows.Next() {
		var (
			p      protocol.Post
			audRaw string
			ts     string
		)
		if err := rows.Scan(&p.ID, &p.TenantID, &p.ParentID, &p.Author, &audRaw, &p.Title, &p.Content, &p.BlobID, &ts); err != nil {
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
		out[p.TenantID] = append(out[p.TenantID], p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate posts: %w", err)
	}
	return out, nil
}

// -- Blob storage --

// SaveBlob stores raw content scoped to tenantID. Content is stored
// base64-encoded so the same TEXT column works across SQLite and PostgreSQL.
func (s *Store) SaveBlob(tenantID, contentType, filename string, content []byte) (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate blob id: %w", err)
	}
	id := hex.EncodeToString(raw[:])
	encoded := base64.StdEncoding.EncodeToString(content)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	q := s.d.rebind(
		`INSERT INTO blobs (id, tenant_id, content, content_type, filename, size, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
	)
	_, err := s.db.Exec(q, id, tenantID, encoded, contentType, filename, int64(len(content)), now)
	if err != nil {
		return "", fmt.Errorf("insert blob: %w", err)
	}
	return id, nil
}

// LoadBlob retrieves the content of the blob with the given ID, scoped to
// tenantID. Returns ErrBlobNotFound when the ID is unknown or belongs to a
// different tenant.
func (s *Store) LoadBlob(tenantID, id string) (content []byte, contentType, filename string, err error) {
	q := s.d.rebind(
		`SELECT content, content_type, filename FROM blobs WHERE id = ? AND tenant_id = ?`,
	)
	var encoded string
	if err := s.db.QueryRow(q, id, tenantID).Scan(&encoded, &contentType, &filename); err != nil {
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

// -- Agent identity tracking --

// UpsertAgent records or updates the operator identity for a named agent
// within a tenant.
func (s *Store) UpsertAgent(tenantID, name, operator string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	q := s.d.rebind(
		`INSERT INTO agents (tenant_id, name, operator, last_seen) VALUES (?, ?, ?, ?)
		 ON CONFLICT(tenant_id, name) DO UPDATE SET operator = excluded.operator, last_seen = excluded.last_seen`,
	)
	_, err := s.db.Exec(q, tenantID, name, operator, now)
	if err != nil {
		return fmt.Errorf("upsert agent %s/%s: %w", tenantID, name, err)
	}
	return nil
}

// -- Maintenance --

// Clear deletes all posts (and optionally keys and tenants) for the given
// tenantID. Pass tenantID="" to clear all tenants. clearKeys also removes
// the keys table rows (and tenant rows when tenantID is empty).
func (s *Store) Clear(tenantID string, clearKeys bool) (posts int, keys int, err error) {
	var res sql.Result
	if tenantID == "" {
		res, err = s.db.Exec(`DELETE FROM posts`)
	} else {
		q := s.d.rebind(`DELETE FROM posts WHERE tenant_id = ?`)
		res, err = s.db.Exec(q, tenantID)
	}
	if err != nil {
		return 0, 0, fmt.Errorf("clear posts: %w", err)
	}
	n, _ := res.RowsAffected()
	posts = int(n)

	if tenantID == "" {
		_, _ = s.db.Exec(`DELETE FROM agents`)
	} else {
		q := s.d.rebind(`DELETE FROM agents WHERE tenant_id = ?`)
		_, _ = s.db.Exec(q, tenantID)
	}

	if clearKeys {
		if tenantID == "" {
			res, err = s.db.Exec(`DELETE FROM keys`)
			if err != nil {
				return posts, 0, fmt.Errorf("clear keys: %w", err)
			}
			n, _ = res.RowsAffected()
			keys = int(n)
			_, _ = s.db.Exec(`DELETE FROM tenants`)
		} else {
			q := s.d.rebind(`DELETE FROM keys WHERE tenant_id = ?`)
			res, err = s.db.Exec(q, tenantID)
			if err != nil {
				return posts, 0, fmt.Errorf("clear keys: %w", err)
			}
			n, _ = res.RowsAffected()
			keys = int(n)
		}
	}
	return posts, keys, nil
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
