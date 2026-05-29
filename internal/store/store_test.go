package store

import (
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

func mustCreateTenant(t *testing.T, s *Store, name string) string {
	t.Helper()
	rec, err := s.CreateTenant(name)
	if err != nil {
		t.Fatalf("CreateTenant(%q): %v", name, err)
	}
	return rec.ID
}

func samplePosts(tenantID string) []protocol.Post {
	t0 := time.Date(2026, 5, 22, 16, 0, 0, 0, time.UTC)
	return []protocol.Post{
		{
			TenantID:  tenantID,
			ID:        "aaaaaaaaaaaaaaaa",
			Author:    "alice",
			Audience:  protocol.Audience{All: true},
			Title:     "greeting",
			Content:   "hello world",
			Timestamp: t0,
		},
		{
			TenantID:  tenantID,
			ID:        "bbbbbbbbbbbbbbbb",
			Author:    "alice",
			Audience:  protocol.Audience{Agents: []string{"alice", "bob"}},
			Title:     "for bob",
			Content:   "psst bob",
			Timestamp: t0.Add(1 * time.Second),
		},
		{
			TenantID:  tenantID,
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
	tid := mustCreateTenant(t, s, "acme")
	posts := samplePosts(tid)
	for _, p := range posts {
		if err := s.Save(p); err != nil {
			t.Fatalf("Save(%s): %v", p.ID, err)
		}
	}
	byTenant, err := s.LoadByTenant()
	if err != nil {
		t.Fatalf("LoadByTenant: %v", err)
	}
	got := byTenant[tid]
	if len(got) != len(posts) {
		t.Fatalf("LoadByTenant returned %d posts for tenant, want %d", len(got), len(posts))
	}
	for i := range got {
		assertEqualPost(t, got[i], posts[i])
	}
}

func TestLoadPreservesPublishOrder(t *testing.T) {
	s := mustOpen(t, ":memory:")
	tid := mustCreateTenant(t, s, "acme")
	posts := []protocol.Post{
		{TenantID: tid, ID: "11", Author: "a", Audience: protocol.Audience{All: true}, Content: "first", Timestamp: time.Unix(2000, 0).UTC()},
		{TenantID: tid, ID: "22", Author: "a", Audience: protocol.Audience{All: true}, Content: "second", Timestamp: time.Unix(1000, 0).UTC()},
		{TenantID: tid, ID: "33", Author: "a", Audience: protocol.Audience{All: true}, Content: "third", Timestamp: time.Unix(3000, 0).UTC()},
	}
	for _, p := range posts {
		if err := s.Save(p); err != nil {
			t.Fatalf("Save(%s): %v", p.ID, err)
		}
	}
	byTenant, err := s.LoadByTenant()
	if err != nil {
		t.Fatalf("LoadByTenant: %v", err)
	}
	got := byTenant[tid]
	wantOrder := []string{"11", "22", "33"}
	for i, p := range got {
		if p.ID != wantOrder[i] {
			t.Errorf("Load order[%d] = %q want %q", i, p.ID, wantOrder[i])
		}
	}
}

func TestLoadEmpty(t *testing.T) {
	s := mustOpen(t, ":memory:")
	byTenant, err := s.LoadByTenant()
	if err != nil {
		t.Fatalf("LoadByTenant: %v", err)
	}
	if len(byTenant) != 0 {
		t.Errorf("LoadByTenant on empty store returned %d tenants: %+v", len(byTenant), byTenant)
	}
}

func TestMultiTenantIsolation(t *testing.T) {
	s := mustOpen(t, ":memory:")
	t1 := mustCreateTenant(t, s, "acme")
	t2 := mustCreateTenant(t, s, "globex")

	p1 := protocol.Post{TenantID: t1, ID: "t1post", Author: "alice", Audience: protocol.Audience{All: true}, Title: "acme post", Content: "for acme", Timestamp: time.Now().UTC()}
	p2 := protocol.Post{TenantID: t2, ID: "t2post", Author: "bob", Audience: protocol.Audience{All: true}, Title: "globex post", Content: "for globex", Timestamp: time.Now().UTC()}

	if err := s.Save(p1); err != nil {
		t.Fatalf("Save t1: %v", err)
	}
	if err := s.Save(p2); err != nil {
		t.Fatalf("Save t2: %v", err)
	}

	byTenant, err := s.LoadByTenant()
	if err != nil {
		t.Fatalf("LoadByTenant: %v", err)
	}
	if len(byTenant[t1]) != 1 || byTenant[t1][0].ID != "t1post" {
		t.Errorf("tenant1 posts = %+v, want [t1post]", byTenant[t1])
	}
	if len(byTenant[t2]) != 1 || byTenant[t2][0].ID != "t2post" {
		t.Errorf("tenant2 posts = %+v, want [t2post]", byTenant[t2])
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "parleyd.db")

	first, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	tid, err := first.CreateTenant("acme")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	posts := samplePosts(tid.ID)
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

	byTenant, err := second.LoadByTenant()
	if err != nil {
		t.Fatalf("LoadByTenant after reopen: %v", err)
	}
	got := byTenant[tid.ID]
	if len(got) != len(posts) {
		t.Fatalf("reopen returned %d posts, want %d", len(got), len(posts))
	}
	for i := range got {
		assertEqualPost(t, got[i], posts[i])
	}
}

func TestDuplicateIDFails(t *testing.T) {
	s := mustOpen(t, ":memory:")
	tid := mustCreateTenant(t, s, "acme")
	posts := samplePosts(tid)
	p := posts[0]
	if err := s.Save(p); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := s.Save(p); err == nil {
		t.Fatal("second Save with duplicate ID succeeded, want primary-key violation")
	}
}

func TestKeyCreateValidateRevoke(t *testing.T) {
	s := mustOpen(t, ":memory:")
	tid := mustCreateTenant(t, s, "acme")

	plaintext, rec, err := s.CreateKey(tid, "test key")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if rec.ID == "" {
		t.Error("CreateKey: ID is empty")
	}
	if rec.TenantID != tid {
		t.Errorf("CreateKey: TenantID = %q, want %q", rec.TenantID, tid)
	}
	if rec.ClientID == "" {
		t.Error("CreateKey: ClientID is empty")
	}
	if rec.DisplayName == "" {
		t.Error("CreateKey: DisplayName is empty")
	}
	if rec.CreatedAt.IsZero() {
		t.Error("CreateKey: CreatedAt is zero")
	}
	if !rec.RevokedAt.IsZero() {
		t.Errorf("CreateKey: new key should not be revoked, got RevokedAt = %v", rec.RevokedAt)
	}
	if len(plaintext) < 10 {
		t.Errorf("CreateKey: plaintext too short: %q", plaintext)
	}

	if !s.ValidateKey(plaintext) {
		t.Error("ValidateKey: active key not recognised")
	}
	if s.ValidateKey("prl_wrong_key") {
		t.Error("ValidateKey: invalid key accepted")
	}

	gotTenant, gotClientID, gotName, ok := s.ClientForKey(plaintext)
	if !ok {
		t.Fatal("ClientForKey: key not found")
	}
	if gotTenant != tid {
		t.Errorf("ClientForKey: tenantID = %q, want %q", gotTenant, tid)
	}
	if gotClientID != rec.ClientID {
		t.Errorf("ClientForKey: clientID = %q, want %q", gotClientID, rec.ClientID)
	}
	if gotName != rec.DisplayName {
		t.Errorf("ClientForKey: displayName = %q, want %q", gotName, rec.DisplayName)
	}

	found, err := s.RevokeKey(rec.ID)
	if err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	if !found {
		t.Error("RevokeKey: returned !found for existing key")
	}
	if s.ValidateKey(plaintext) {
		t.Error("ValidateKey: revoked key still accepted")
	}

	found, err = s.RevokeKey(rec.ID)
	if err != nil {
		t.Fatalf("RevokeKey second call: %v", err)
	}
	if found {
		t.Error("RevokeKey: second revoke returned found=true, want false")
	}
}

func TestKeyListShowsAllStates(t *testing.T) {
	s := mustOpen(t, ":memory:")
	tid := mustCreateTenant(t, s, "acme")

	_, rec1, err := s.CreateKey(tid, "key for alice")
	if err != nil {
		t.Fatalf("CreateKey alice: %v", err)
	}
	_, rec2, err := s.CreateKey(tid, "key for bob")
	if err != nil {
		t.Fatalf("CreateKey bob: %v", err)
	}
	if _, err := s.RevokeKey(rec2.ID); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}

	keys, err := s.ListKeys(tid)
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("ListKeys: got %d, want 2", len(keys))
	}

	byID := make(map[string]KeyRecord)
	for _, k := range keys {
		byID[k.ID] = k
	}
	if k, ok := byID[rec1.ID]; !ok || !k.RevokedAt.IsZero() {
		t.Errorf("active key: RevokedAt should be zero, got %v", byID[rec1.ID].RevokedAt)
	}
	if k, ok := byID[rec2.ID]; !ok || k.RevokedAt.IsZero() {
		t.Errorf("revoked key: RevokedAt should be set, got zero for %v", k)
	}
}

func TestRenameClientRoundTrip(t *testing.T) {
	s := mustOpen(t, ":memory:")
	tid := mustCreateTenant(t, s, "acme")

	_, rec, err := s.CreateKey(tid, "laptop")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	if err := s.RenameClient(tid, rec.ClientID, "hawk"); err != nil {
		t.Fatalf("RenameClient: %v", err)
	}

	clients, err := s.ListClients(tid)
	if err != nil {
		t.Fatalf("ListClients: %v", err)
	}
	if len(clients) != 1 {
		t.Fatalf("ListClients: got %d clients, want 1", len(clients))
	}
	if clients[0].DisplayName != "hawk" {
		t.Errorf("DisplayName = %q, want hawk", clients[0].DisplayName)
	}
	if clients[0].ClientID != rec.ClientID {
		t.Errorf("ClientID = %q, want %q", clients[0].ClientID, rec.ClientID)
	}
}
