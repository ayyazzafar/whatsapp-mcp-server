package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestTrigger(t *testing.T) {
	ctx := context.Background()
	db, dialect, err := OpenDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := NewStore(ctx, db, dialect)
	if err != nil {
		t.Fatal(err)
	}
	chats, _ := ParseAllowList("120363000000000001@g.us")
	from, _ := ParseAllowList("447700900123")
	chat, _ := types.ParseJID("120363000000000001@g.us")
	owner := types.NewJID("447700900123", types.DefaultUserServer)
	other := types.NewJID("15550001111", types.DefaultUserServer)
	me := types.NewJID("15550002222", types.DefaultUserServer)

	fired := make(chan string, 4)
	newTrig := func() *Trigger {
		tr := NewTrigger(TriggerConfig{Format: "claude-routine", Chats: chats, From: from, Delay: 50 * time.Millisecond, PerHour: 5}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
		tr.post = func(_ context.Context, body []byte) error { fired <- string(body); return nil }
		return tr
	}
	n := 0
	say := func(tr *Trigger, sender types.JID, fromMe bool, text string, at time.Time) {
		n++
		_ = st.SaveMessage(ctx, &StoredMessage{ChatJID: chat.String(), ID: fmt.Sprint("m", n), SenderJID: sender.String(), FromMe: fromMe, Text: text, ts: at.Unix()})
		tr.Observe(chat, sender, fromMe, at)
	}
	expect := func(want bool, label string) {
		select {
		case body := <-fired:
			if !want {
				t.Fatalf("%s: unexpected fire %s", label, body)
			}
			if !strings.Contains(body, "NEW") || !strings.Contains(body, "hello claude") {
				t.Fatalf("%s: payload missing new message: %s", label, body)
			}
		case <-time.After(200 * time.Millisecond):
			if want {
				t.Fatalf("%s: did not fire", label)
			}
		}
	}

	tr := newTrig()
	say(tr, owner, false, "hello claude", time.Now())
	expect(true, "unanswered owner message")

	tr = newTrig()
	say(tr, owner, false, "hello claude again", time.Now().Add(time.Second))
	say(tr, me, true, "answered from the desktop", time.Now().Add(2*time.Second))
	expect(false, "answered by another device")

	tr = newTrig()
	say(tr, other, false, "hello claude from someone else", time.Now().Add(3*time.Second))
	expect(false, "sender not allowed")

	tr = newTrig()
	say(tr, owner, false, "hello claude, old backlog", time.Now().Add(-time.Hour))
	expect(false, "old backlog message")
}
