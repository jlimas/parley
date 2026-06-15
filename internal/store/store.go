// Package store persists parley posts, API keys, tenants, and client identities.
// The broker keeps the in-memory hub as its read path; this package handles
// durability so a restart does not wipe the board.
//
// Every resource (post, key, blob) belongs to a tenant. Queries are always
// scoped to a tenant_id so boards are fully isolated.
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

	"github.com/jlimas/parley/internal/names"
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
    id           TEXT PRIMARY KEY,
    tenant_id    TEXT NOT NULL REFERENCES tenants(id),
    agent        TEXT NOT NULL DEFAULT '',
    key_hash     TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    revoked_at   TEXT,
    client_id    TEXT NOT NULL DEFAULT '',
    display_name TEXT NOT NULL DEFAULT '',
    description  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS keys_hash ON keys(key_hash);
CREATE INDEX IF NOT EXISTS keys_client ON keys(client_id);

CREATE TABLE IF NOT EXISTS posts (
    id         TEXT    PRIMARY KEY,
    tenant_id  TEXT    NOT NULL,
    parent_id  TEXT    NOT NULL DEFAULT '',
    author     TEXT    NOT NULL,
    audience   TEXT    NOT NULL,
    title      TEXT    NOT NULL DEFAULT '',
    content    TEXT    NOT NULL DEFAULT '',
    blob_id    TEXT    NOT NULL DEFAULT '',
    blob_name  TEXT    NOT NULL DEFAULT '',
    timestamp  TEXT    NOT NULL,
    seq        INTEGER NOT NULL,
    edited_at  TEXT    NOT NULL DEFAULT ''
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

// Open opens the database at dsn, creates the schema if missing, applies
// additive column migrations, and returns a Store ready for use.
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
	if err := applyMigrations(db, d); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrateLegacyClients(db, d); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, d: d}, nil
}

// applyMigrations runs additive ALTER TABLE migrations. Each statement is
// idempotent: Postgres uses ADD COLUMN IF NOT EXISTS; SQLite silently ignores
// the "duplicate column" error that fires when the column already exists.
func applyMigrations(db *sql.DB, d dialect) error {
	addCols := []struct{ table, col, def string }{
		{"posts", "blob_name", "TEXT NOT NULL DEFAULT ''"},
		{"posts", "edited_at", "TEXT NOT NULL DEFAULT ''"},
		{"keys", "client_id", "TEXT NOT NULL DEFAULT ''"},
		{"keys", "display_name", "TEXT NOT NULL DEFAULT ''"},
		{"keys", "description", "TEXT NOT NULL DEFAULT ''"},
	}
	for _, c := range addCols {
		stmt := fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, c.table, c.col, c.def)
		if _, ok := d.(postgresDialect); ok {
			stmt = fmt.Sprintf(`ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s`, c.table, c.col, c.def)
		}
		if _, err := db.Exec(stmt); err != nil && !isDuplicateColumn(err) {
			return fmt.Errorf("migration add %s.%s: %w", c.table, c.col, err)
		}
	}
	return nil
}

// migrateLegacyClients assigns client_id and display_name to existing keys
// created before the identity-model simplification. For each legacy key (where
// client_id is empty) the old agent value becomes the display_name and a new
// random base-36 ID is generated. Posts authored or addressed under the old
// agent name are rewritten to use the new client_id.
func migrateLegacyClients(db *sql.DB, d dialect) error {
	rows, err := db.Query(`SELECT id, tenant_id, agent FROM keys WHERE client_id = ''`)
	if err != nil {
		return fmt.Errorf("migrate clients: query legacy keys: %w", err)
	}
	type legacyKey struct{ id, tenantID, agent string }
	var legacy []legacyKey
	for rows.Next() {
		var k legacyKey
		if err := rows.Scan(&k.id, &k.tenantID, &k.agent); err != nil {
			rows.Close()
			return fmt.Errorf("migrate clients: scan key: %w", err)
		}
		legacy = append(legacy, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("migrate clients: iterate keys: %w", err)
	}

	for _, k := range legacy {
		clientID := names.GenClientID()
		displayName := k.agent
		if displayName == "" {
			displayName = names.Pick()
		}

		uq := d.rebind(`UPDATE keys SET client_id = ?, display_name = ? WHERE id = ?`)
		if _, err := db.Exec(uq, clientID, displayName, k.id); err != nil {
			return fmt.Errorf("migrate clients: update key %s: %w", k.id, err)
		}

		if k.agent == "" {
			continue
		}

		// Rewrite posts.author for this tenant.
		aq := d.rebind(`UPDATE posts SET author = ? WHERE author = ? AND tenant_id = ?`)
		if _, err := db.Exec(aq, clientID, k.agent, k.tenantID); err != nil {
			return fmt.Errorf("migrate clients: rewrite author for %s: %w", k.agent, err)
		}

		// Rewrite audience JSON entries that name this agent.
		// We load each post that could contain the old name and rewrite in Go.
		pq := d.rebind(`SELECT id, audience FROM posts WHERE tenant_id = ? AND audience LIKE ?`)
		prows, err := db.Query(pq, k.tenantID, "%"+k.agent+"%")
		if err != nil {
			return fmt.Errorf("migrate clients: query posts for %s: %w", k.agent, err)
		}
		type audRow struct{ id, audience string }
		var audRows []audRow
		for prows.Next() {
			var r audRow
			if err := prows.Scan(&r.id, &r.audience); err != nil {
				prows.Close()
				return fmt.Errorf("migrate clients: scan post audience: %w", err)
			}
			audRows = append(audRows, r)
		}
		prows.Close()
		if err := prows.Err(); err != nil {
			return fmt.Errorf("migrate clients: iterate post audiences: %w", err)
		}
		for _, ar := range audRows {
			var aud protocol.Audience
			if err := json.Unmarshal([]byte(ar.audience), &aud); err != nil {
				continue
			}
			changed := false
			for i, a := range aud.Agents {
				if a == k.agent {
					aud.Agents[i] = clientID
					changed = true
				}
			}
			if !changed {
				continue
			}
			updated, err := json.Marshal(aud)
			if err != nil {
				continue
			}
			uaq := d.rebind(`UPDATE posts SET audience = ? WHERE id = ?`)
			if _, err := db.Exec(uaq, string(updated), ar.id); err != nil {
				return fmt.Errorf("migrate clients: update audience for post %s: %w", ar.id, err)
			}
		}
	}
	return nil
}

// isDuplicateColumn returns true for SQLite's "duplicate column name" error,
// which fires when ADD COLUMN targets a column that already exists.
func isDuplicateColumn(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate column name")
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
	ID          string
	TenantID    string
	ClientID    string // short base-36 identity (auto-generated at key creation)
	DisplayName string // human-readable name (auto-assigned, user-renameable)
	Description string // admin note passed at key creation
	CreatedAt   time.Time
	RevokedAt   time.Time // zero = active
}

// ClientRecord is the public identity view of a client (key holder).
type ClientRecord struct {
	ClientID    string
	DisplayName string
}

// CreateKey mints a new API key for the given tenant. A random client_id and
// display_name are assigned automatically; description is an admin note.
// The plaintext key is printed once and never stored.
func (s *Store) CreateKey(tenantID, description string) (plaintext string, rec KeyRecord, err error) {
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
	clientID := names.GenClientID()
	displayName := names.Pick()
	hash := sha256Hex(plaintext)
	now := time.Now().UTC()

	q := s.d.rebind(`INSERT INTO keys (id, tenant_id, agent, client_id, display_name, description, key_hash, created_at) VALUES (?, ?, '', ?, ?, ?, ?, ?)`)
	if _, err := s.db.Exec(q, id, tenantID, clientID, displayName, description, hash, now.Format(time.RFC3339Nano)); err != nil {
		return "", KeyRecord{}, fmt.Errorf("insert key: %w", err)
	}
	return plaintext, KeyRecord{
		ID: id, TenantID: tenantID,
		ClientID: clientID, DisplayName: displayName, Description: description,
		CreatedAt: now,
	}, nil
}

// ListKeys returns all keys for the given tenant ordered by creation time.
func (s *Store) ListKeys(tenantID string) ([]KeyRecord, error) {
	q := s.d.rebind(`SELECT id, tenant_id, client_id, display_name, description, created_at, revoked_at FROM keys WHERE tenant_id = ? ORDER BY created_at ASC`)
	rows, err := s.db.Query(q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("query keys: %w", err)
	}
	defer rows.Close()
	return scanKeys(rows)
}

// ListAllKeys returns all keys across all tenants.
func (s *Store) ListAllKeys() ([]KeyRecord, error) {
	rows, err := s.db.Query(`SELECT id, tenant_id, client_id, display_name, description, created_at, revoked_at FROM keys ORDER BY tenant_id, created_at ASC`)
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
		if err := rows.Scan(&rec.ID, &rec.TenantID, &rec.ClientID, &rec.DisplayName, &rec.Description, &created, &revokedS); err != nil {
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

// ClientForKey resolves a raw API key to its (tenantID, clientID, displayName)
// triple. Returns ("", "", "", false) if the key is unknown or revoked.
func (s *Store) ClientForKey(key string) (tenantID, clientID, displayName string, ok bool) {
	hash := sha256Hex(key)
	q := s.d.rebind(`SELECT tenant_id, client_id, display_name FROM keys WHERE key_hash = ? AND revoked_at IS NULL`)
	err := s.db.QueryRow(q, hash).Scan(&tenantID, &clientID, &displayName)
	if err != nil {
		return "", "", "", false
	}
	return tenantID, clientID, displayName, true
}

// RenameClient updates the display_name for the client identified by clientID
// within the given tenant. Returns an error if the client is not found.
func (s *Store) RenameClient(tenantID, clientID, newName string) error {
	q := s.d.rebind(`UPDATE keys SET display_name = ? WHERE tenant_id = ? AND client_id = ? AND revoked_at IS NULL`)
	res, err := s.db.Exec(q, newName, tenantID, clientID)
	if err != nil {
		return fmt.Errorf("rename client: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("rename client: client %s not found in tenant %s", clientID, tenantID)
	}
	return nil
}

// ListClients returns all active clients (distinct client_id + display_name
// pairs) for the given tenant, ordered by display_name.
func (s *Store) ListClients(tenantID string) ([]ClientRecord, error) {
	q := s.d.rebind(`SELECT DISTINCT client_id, display_name FROM keys WHERE tenant_id = ? AND revoked_at IS NULL AND client_id != '' ORDER BY display_name ASC`)
	rows, err := s.db.Query(q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list clients: %w", err)
	}
	defer rows.Close()
	var out []ClientRecord
	for rows.Next() {
		var r ClientRecord
		if err := rows.Scan(&r.ClientID, &r.DisplayName); err != nil {
			return nil, fmt.Errorf("scan client: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ClientByName finds a client by display_name within the given tenant.
// Returns (zero, false, nil) when no match is found.
func (s *Store) ClientByName(tenantID, name string) (ClientRecord, bool, error) {
	q := s.d.rebind(`SELECT client_id, display_name FROM keys WHERE tenant_id = ? AND display_name = ? AND revoked_at IS NULL LIMIT 1`)
	var r ClientRecord
	err := s.db.QueryRow(q, tenantID, name).Scan(&r.ClientID, &r.DisplayName)
	if err == sql.ErrNoRows {
		return ClientRecord{}, false, nil
	}
	if err != nil {
		return ClientRecord{}, false, fmt.Errorf("client by name: %w", err)
	}
	return r, true, nil
}

// -- Post persistence --

// Save persists p. p.TenantID, p.ID, and p.Timestamp must be populated.
func (s *Store) Save(p protocol.Post) error {
	aud, err := json.Marshal(p.Audience)
	if err != nil {
		return fmt.Errorf("marshal audience: %w", err)
	}
	q := s.d.rebind(
		`INSERT INTO posts (id, tenant_id, parent_id, author, audience, title, content, blob_id, blob_name, timestamp, seq, edited_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		         (SELECT COALESCE(MAX(seq), 0) + 1 FROM posts WHERE tenant_id = ?), '')`,
	)
	_, err = s.db.Exec(q,
		p.ID, p.TenantID, p.ParentID, p.Author, string(aud),
		p.Title, p.Content, p.BlobID, p.BlobName,
		p.Timestamp.UTC().Format(time.RFC3339Nano),
		p.TenantID,
	)
	if err != nil {
		return fmt.Errorf("insert post %s: %w", p.ID, err)
	}
	return nil
}

// UpdatePost persists content and title changes for an existing post.
// p.ID, p.TenantID, p.Author, and p.EditedAt must be set; ownership is
// verified at the DB layer as a belt-and-suspenders check (the hub already
// enforces it).
func (s *Store) UpdatePost(p protocol.Post) error {
	editedAt := ""
	if p.EditedAt != nil {
		editedAt = p.EditedAt.UTC().Format(time.RFC3339Nano)
	}
	q := s.d.rebind(`UPDATE posts SET content = ?, title = ?, edited_at = ? WHERE id = ? AND tenant_id = ? AND author = ?`)
	res, err := s.db.Exec(q, p.Content, p.Title, editedAt, p.ID, p.TenantID, p.Author)
	if err != nil {
		return fmt.Errorf("update post %s: %w", p.ID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("update post %s: not found or not owner", p.ID)
	}
	return nil
}

// DeletePost removes a post by ID within a tenant. Ownership and reply
// constraints must be verified by the caller before invoking this method.
func (s *Store) DeletePost(tenantID, id string) error {
	q := s.d.rebind(`DELETE FROM posts WHERE id = ? AND tenant_id = ?`)
	_, err := s.db.Exec(q, id, tenantID)
	if err != nil {
		return fmt.Errorf("delete post %s: %w", id, err)
	}
	return nil
}

// LoadByTenant returns every stored post grouped by tenant_id, each tenant's
// slice ordered by seq (publish order).
func (s *Store) LoadByTenant() (map[string][]protocol.Post, error) {
	rows, err := s.db.Query(
		`SELECT id, tenant_id, parent_id, author, audience, title, content, blob_id, blob_name, timestamp, edited_at
		 FROM posts ORDER BY tenant_id, seq ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("query posts: %w", err)
	}
	defer rows.Close()

	out := make(map[string][]protocol.Post)
	for rows.Next() {
		var (
			p        protocol.Post
			audRaw   string
			ts       string
			editedAt string
		)
		if err := rows.Scan(&p.ID, &p.TenantID, &p.ParentID, &p.Author, &audRaw, &p.Title, &p.Content, &p.BlobID, &p.BlobName, &ts, &editedAt); err != nil {
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
		if editedAt != "" {
			t, err := time.Parse(time.RFC3339Nano, editedAt)
			if err != nil {
				return nil, fmt.Errorf("parse edited_at for %s: %w", p.ID, err)
			}
			ut := t.UTC()
			p.EditedAt = &ut
		}
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

// BlobFilename returns just the original filename for a blob without loading
// its content. Returns ErrBlobNotFound when the ID is unknown or belongs to a
// different tenant.
func (s *Store) BlobFilename(tenantID, id string) (string, error) {
	q := s.d.rebind(`SELECT filename FROM blobs WHERE id = ? AND tenant_id = ?`)
	var filename string
	if err := s.db.QueryRow(q, id, tenantID).Scan(&filename); err != nil {
		if err == sql.ErrNoRows {
			return "", ErrBlobNotFound
		}
		return "", fmt.Errorf("blob filename %s: %w", id, err)
	}
	return filename, nil
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
