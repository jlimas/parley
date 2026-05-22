package protocol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseAudience(t *testing.T) {
	cases := []struct {
		in      string
		want    Audience
		wantErr bool
	}{
		{in: "all", want: Audience{All: true}},
		{in: "@alice", want: Audience{Agents: []string{"alice"}}},
		{in: "@bob_2", want: Audience{Agents: []string{"bob_2"}}},
		{in: "@alice,@bob", want: Audience{Agents: []string{"alice", "bob"}}},
		{in: "@alice,@bob,@carol", want: Audience{Agents: []string{"alice", "bob", "carol"}}},
		{in: "@alice, @bob", want: Audience{Agents: []string{"alice", "bob"}}},
		{in: "@alice,@alice,@bob", want: Audience{Agents: []string{"alice", "bob"}}},
		{in: "", wantErr: true},
		{in: "@", wantErr: true},
		{in: "alice", wantErr: true},
		{in: "ALL", wantErr: true},
		{in: "@alice,", wantErr: true},
		{in: ",@alice", wantErr: true},
		{in: "@alice,,@bob", wantErr: true},
		{in: "@alice,bob", wantErr: true},
		{in: "all,@alice", wantErr: true},
		{in: "@alice,all", wantErr: true},
		{in: "@alice,@", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseAudience(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseAudience(%q): expected error, got %+v", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseAudience(%q): unexpected error: %v", tc.in, err)
			}
			if got.All != tc.want.All {
				t.Errorf("All mismatch: got %v want %v", got.All, tc.want.All)
			}
			if len(got.Agents) != len(tc.want.Agents) {
				t.Fatalf("Agents length mismatch: got %v want %v", got.Agents, tc.want.Agents)
			}
			for i := range got.Agents {
				if got.Agents[i] != tc.want.Agents[i] {
					t.Errorf("Agents[%d] = %q want %q", i, got.Agents[i], tc.want.Agents[i])
				}
			}
		})
	}
}

func TestAudienceStringRoundTrip(t *testing.T) {
	cases := []string{"all", "@alice", "@bob", "@alice,@bob", "@alice,@bob,@carol"}
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			a, err := ParseAudience(s)
			if err != nil {
				t.Fatalf("ParseAudience(%q): %v", s, err)
			}
			if got := a.String(); got != s {
				t.Errorf("round-trip: ParseAudience(%q).String() = %q", s, got)
			}
		})
	}
}

func TestAudienceStringMultiAgent(t *testing.T) {
	a := Audience{Agents: []string{"alice", "bob"}}
	if got := a.String(); got != "@alice,@bob" {
		t.Errorf("multi-agent String = %q want %q", got, "@alice,@bob")
	}
}

func TestPostJSONTitleRoundTrip(t *testing.T) {
	// Title is the new headline field; ensure it survives a marshal/unmarshal
	// cycle and that omitempty drops it from the wire when empty (so legacy
	// clients that don't know the field don't see noise).
	full := Post{
		ID:        "abc123",
		Author:    "alice",
		Audience:  Audience{All: true},
		Title:     "headline",
		Content:   "body",
		Timestamp: time.Date(2026, 5, 22, 16, 0, 0, 0, time.UTC),
	}
	b, err := json.Marshal(full)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"title":"headline"`) {
		t.Errorf("marshaled JSON missing title: %s", b)
	}
	var back Post
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Title != "headline" {
		t.Errorf("Title round-trip = %q, want %q", back.Title, "headline")
	}

	reply := Post{
		ID:       "def456",
		ParentID: "abc123",
		Author:   "bob",
		Audience: Audience{All: true},
		Content:  "got it",
	}
	rb, err := json.Marshal(reply)
	if err != nil {
		t.Fatalf("marshal reply: %v", err)
	}
	if strings.Contains(string(rb), `"title"`) {
		t.Errorf("empty title should be omitted from JSON, got %s", rb)
	}
}

func TestAudienceIncludes(t *testing.T) {
	cases := []struct {
		name     string
		audience Audience
		agent    string
		want     bool
	}{
		{name: "all matches anyone", audience: Audience{All: true}, agent: "alice", want: true},
		{name: "all matches empty too", audience: Audience{All: true}, agent: "", want: true},
		{name: "listed agent matches", audience: Audience{Agents: []string{"alice", "bob"}}, agent: "bob", want: true},
		{name: "unlisted agent rejected", audience: Audience{Agents: []string{"alice"}}, agent: "bob", want: false},
		{name: "empty audience rejects everyone", audience: Audience{}, agent: "alice", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.audience.Includes(tc.agent); got != tc.want {
				t.Errorf("Includes(%q) = %v want %v (audience=%+v)", tc.agent, got, tc.want, tc.audience)
			}
		})
	}
}
