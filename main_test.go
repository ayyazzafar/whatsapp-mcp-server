package main

import (
	"context"
	"path/filepath"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

func TestAllowList(t *testing.T) {
	a, err := ParseAllowList("120363000000000001@g.us, +44 7700 900123")
	if err != nil {
		t.Fatal(err)
	}
	group, _ := types.ParseJID("120363000000000001@g.us")
	if !a.Allows(group) || !a.Allows(types.NewJID("447700900123", types.DefaultUserServer)) {
		t.Fatal("expected listed chats to be allowed")
	}
	if a.Allows(types.NewJID("15550001111", types.DefaultUserServer)) {
		t.Fatal("unlisted chat allowed")
	}
	if all, _ := ParseAllowList("*"); !all.Allows(group) {
		t.Fatal("* should allow everything")
	}
	if none, _ := ParseAllowList(""); !none.Empty() || none.Allows(group) {
		t.Fatal("empty list should allow nothing")
	}
	if _, err := ParseAllowList("hello"); err == nil {
		t.Fatal("garbage entry accepted")
	}
}

func TestRateLimiter(t *testing.T) {
	r := &RateLimiter{limit: 2}
	if r.Take() != nil || r.Take() != nil {
		t.Fatal("first two sends should pass")
	}
	if r.Take() == nil || r.Remaining() != 0 {
		t.Fatal("third send should be refused")
	}
}

func TestRedirectAllowed(t *testing.T) {
	o := &OAuth{cfg: &Config{RedirectHosts: []string{"claude.ai", "localhost"}}}
	for uri, want := range map[string]bool{
		"https://claude.ai/api/mcp/auth_callback": true,
		"https://sub.claude.ai/cb":                true,
		"http://localhost:3334/callback":          true,
		"http://claude.ai/cb":                     false, // http only on loopback
		"https://evilclaude.ai/cb":                false,
		"https://claude.ai.evil.com/cb":           false,
		"javascript:alert(1)":                     false,
		"https://user@claude.ai/cb":               false,
	} {
		if got := o.redirectAllowed(uri); got != want {
			t.Errorf("%s: got %v want %v", uri, got, want)
		}
	}
}

func TestStoreSQLite(t *testing.T) {
	ctx := context.Background()
	db, dialect, err := OpenDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := NewStore(ctx, db, dialect)
	if err != nil {
		t.Fatal(err)
	}
	for i, txt := range []string{"hello world", "deploy is green", "Hello again"} {
		if err := st.SaveMessage(ctx, &StoredMessage{ChatJID: "1@g.us", ID: string(rune('a' + i)), SenderJID: "x@s.whatsapp.net", Text: txt, ts: int64(100 + i)}); err != nil {
			t.Fatal(err)
		}
	}
	msgs, err := st.Messages(ctx, "1@g.us", 10, 0, 0)
	if err != nil || len(msgs) != 3 || msgs[0].Text != "Hello again" {
		t.Fatalf("messages: %v %+v", err, msgs)
	}
	found, err := st.Search(ctx, "HELLO", []string{"1@g.us"}, 10)
	if err != nil || len(found) != 2 {
		t.Fatalf("search: %v %d", err, len(found))
	}
	_ = st.SetChatName(ctx, "1@g.us", "Team")
	if j, _ := st.FindChatByName(ctx, "team"); j != "1@g.us" {
		t.Fatalf("find by name: %q", j)
	}
}
