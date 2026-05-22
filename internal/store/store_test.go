package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/jlimas/parley/internal/protocol"
)

func mustOpen(t *testing.T, dsn string) *Store {
	t.Helper()
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open(%q): %v", dsn, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func samplePosts() []protocol.Post {
	t0 := time.Date(2026, 5, 22, 16, 0, 0, 0, time.UTC)
	return []protocol.Post{
		{
			ID:        "aaaaaaaaaaaaaaaa",
			Author:    "alice",
			Audience:  protocol.Audience{All: true},
			Title:     "greeting",
			Content:   "hello world",
			Timestamp: t0,
		},
		{
			ID:        "bbbbbbbbbbbbbbbb",
			Author:    "alice",
			Audience:  protocol.Audience{Agents: []string{"alice", "bob"}},
			Title:     "for bob",
			Content:   "psst bob",
			Timestamp: t0.Add(1 * time.Second),
		},
		{
			ID:        "cccccccccccccccc",
			ParentID:  "bbbbbbbbbbbbbbbb",
			Author:    "bob",
			Audience:  protocol.Audience{Agents: []string{"alice", "bob"}},
			Content:   "got it",
			Timestamp: t0.Add(2 * time.Second),
		},
	}
}

func assertEqualPost(t *testing.T, got, want protocol.Post) {
	t.Helper()
	if got.ID != want.ID {
		t.Errorf("ID: got %q want %q", got.ID, want.ID)
	}
	if got.ParentID != want.ParentID {
		t.Errorf("ParentID: got %q want %q", got.ParentID, want.ParentID)
	}
	if got.Author != want.Author {
		t.Errorf("Author: got %q want %q", got.Author, want.Author)
	}
	if got.Title != want.Title {
		t.Errorf("Title: got %q want %q", got.Title, want.Title)
	}
	if got.Content != want.Content {
		t.Errorf("Content: got %q want %q", got.Content, want.Content)
	}
	if got.Audience.All != want.Audience.All {
		t.Errorf("Audience.All: got %v want %v", got.Audience.All, want.Audience.All)
	}
	if len(got.Audience.Agents) != len(want.Audience.Agents) {
		t.Errorf("Audience.Agents length: got %v want %v", got.Audience.Agents, want.Audience.Agents)
	} else {
		for i := range got.Audience.Agents {
			if got.Audience.Agents[i] != want.Audience.Agents[i] {
				t.Errorf("Audience.Agents[%d]: got %q want %q", i, got.Audience.Agents[i], want.Audience.Agents[i])
			}
		}
	}
	if !got.Timestamp.Equal(want.Timestamp) {
		t.Errorf("Timestamp: got %s want %s", got.Timestamp, want.Timestamp)
	}
}

func TestSaveLoadRoundTripMemory(t *testing.T) {
	s := mustOpen(t, ":memory:")
	posts := samplePosts()
	for _, p := range posts {
		if err := s.Save(p); err != nil {
			t.Fatalf("Save(%s): %v", p.ID, err)
		}
	}
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != len(posts) {
		t.Fatalf("Load returned %d posts, want %d", len(got), len(posts))
	}
	for i := range got {
		assertEqualPost(t, got[i], posts[i])
	}
}

func TestLoadPreservesPublishOrder(t *testing.T) {
	// Insert posts whose Timestamps are deliberately out of order — Load
	// must order by seq (insertion order), not timestamp, so that replay
	// gives the hub the same sequence the broker originally saw.
	s := mustOpen(t, ":memory:")
	posts := []protocol.Post{
		{ID: "11", Author: "a", Audience: protocol.Audience{All: true}, Content: "first", Timestamp: time.Unix(2000, 0).UTC()},
		{ID: "22", Author: "a", Audience: protocol.Audience{All: true}, Content: "second", Timestamp: time.Unix(1000, 0).UTC()},
		{ID: "33", Author: "a", Audience: protocol.Audience{All: true}, Content: "third", Timestamp: time.Unix(3000, 0).UTC()},
	}
	for _, p := range posts {
		if err := s.Save(p); err != nil {
			t.Fatalf("Save(%s): %v", p.ID, err)
		}
	}
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	wantOrder := []string{"11", "22", "33"}
	for i, p := range got {
		if p.ID != wantOrder[i] {
			t.Errorf("Load order[%d] = %q want %q", i, p.ID, wantOrder[i])
		}
	}
}

func TestLoadEmpty(t *testing.T) {
	s := mustOpen(t, ":memory:")
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Load on empty store returned %d posts: %+v", len(got), got)
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "parleyd.db")

	first, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	posts := samplePosts()
	for _, p := range posts {
		if err := first.Save(p); err != nil {
			t.Fatalf("Save(%s): %v", p.ID, err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	got, err := second.Load()
	if err != nil {
		t.Fatalf("Load after reopen: %v", err)
	}
	if len(got) != len(posts) {
		t.Fatalf("reopen returned %d posts, want %d", len(got), len(posts))
	}
	for i := range got {
		assertEqualPost(t, got[i], posts[i])
	}
}

func TestMigrationAddsTitleColumn(t *testing.T) {
	// Simulate a database created before the title column existed: insert a
	// row using the pre-migration schema, then re-Open via the production
	// path and verify the column is added and the legacy row loads with an
	// empty title.
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	raw.SetMaxOpenConns(1)
	const legacySchema = `
CREATE TABLE posts (
    id         TEXT    PRIMARY KEY,
    parent_id  TEXT    NOT NULL DEFAULT '',
    author     TEXT    NOT NULL,
    audience   TEXT    NOT NULL,
    content    TEXT    NOT NULL,
    timestamp  TEXT    NOT NULL,
    seq        INTEGER NOT NULL
);`
	if _, err := raw.Exec(legacySchema); err != nil {
		t.Fatalf("legacy schema: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO posts (id, parent_id, author, audience, content, timestamp, seq)
		 VALUES ('legacy01', '', 'alice', '{"all":true}', 'old post', '2026-05-22T16:00:00Z', 1)`,
	); err != nil {
		t.Fatalf("legacy insert: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}

	s := mustOpen(t, path)
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load after migration: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Load returned %d posts, want 1", len(got))
	}
	if got[0].Title != "" {
		t.Errorf("migrated row Title = %q, want \"\"", got[0].Title)
	}
	if got[0].Content != "old post" {
		t.Errorf("migrated row Content = %q, want %q", got[0].Content, "old post")
	}

	// A fresh Save through the migrated store should round-trip the title.
	if err := s.Save(protocol.Post{
		ID:        "new01",
		Author:    "alice",
		Audience:  protocol.Audience{All: true},
		Title:     "fresh",
		Content:   "with title",
		Timestamp: time.Date(2026, 5, 22, 17, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("Save after migration: %v", err)
	}
	got, err = s.Load()
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	if len(got) != 2 || got[1].Title != "fresh" {
		t.Errorf("post-migration Title not persisted, got %+v", got)
	}
}

func TestMigrationIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "twice.db")
	first := mustOpen(t, path)
	if err := first.Save(protocol.Post{
		ID:        "x1",
		Author:    "alice",
		Audience:  protocol.Audience{All: true},
		Title:     "first",
		Content:   "body",
		Timestamp: time.Date(2026, 5, 22, 18, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// mustOpen registers a Close via t.Cleanup; reopening via the production
	// path on the same file must not error or lose data.
	second := mustOpen(t, path)
	got, err := second.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[0].Title != "first" {
		t.Errorf("after re-open, posts = %+v", got)
	}
}

func TestDuplicateIDFails(t *testing.T) {
	// The id PRIMARY KEY is what protects the broker from replaying or
	// double-recording a post. If this stops failing, the schema or the
	// Save path lost that guarantee.
	s := mustOpen(t, ":memory:")
	p := samplePosts()[0]
	if err := s.Save(p); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := s.Save(p); err == nil {
		t.Fatal("second Save with duplicate ID succeeded, want primary-key violation")
	}
}
