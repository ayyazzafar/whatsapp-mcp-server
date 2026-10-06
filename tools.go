package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.mau.fi/whatsmeow/types"
)

const version = "0.1.0"

// RateLimiter is a sliding one-hour window shared by every send tool.
type RateLimiter struct {
	mu    sync.Mutex
	limit int
	sent  []time.Time
}

func (r *RateLimiter) Take() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := time.Now().Add(-time.Hour)
	i := 0
	for i < len(r.sent) && r.sent[i].Before(cutoff) {
		i++
	}
	r.sent = r.sent[i:]
	if len(r.sent) >= r.limit {
		wait := time.Until(r.sent[0].Add(time.Hour)).Round(time.Minute)
		return fmt.Errorf("send limit reached (%d per hour, WA_SEND_PER_HOUR); next slot in %s", r.limit, wait)
	}
	r.sent = append(r.sent, time.Now())
	return nil
}

func (r *RateLimiter) Remaining() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := time.Now().Add(-time.Hour)
	n := 0
	for _, t := range r.sent {
		if t.After(cutoff) {
			n++
		}
	}
	return r.limit - n
}

type tools struct {
	wa   *WA
	st   *Store
	cfg  *Config
	rate *RateLimiter
}

// resolveChat accepts a JID, a phone number with country code, or the exact
// name of a chat the server has seen.
func (t *tools) resolveChat(ctx context.Context, chat string) (types.JID, error) {
	chat = strings.TrimSpace(chat)
	switch {
	case chat == "":
		return types.JID{}, errors.New("chat is required")
	case strings.Contains(chat, "@"):
		jid, err := types.ParseJID(chat)
		if err != nil {
			return jid, fmt.Errorf("bad JID: %w", err)
		}
		return t.wa.normalize(ctx, jid), nil
	case len(digitsOnly(chat)) >= 7 && len(digitsOnly(chat)) >= len(strings.Trim(chat, "+ -()"))-4:
		return types.NewJID(digitsOnly(chat), types.DefaultUserServer), nil
	}
	j, err := t.st.FindChatByName(ctx, chat)
	if err != nil {
		return types.JID{}, err
	}
	if j == "" {
		return types.JID{}, fmt.Errorf("no chat named %q; use list_chats to see JIDs", chat)
	}
	return types.ParseJID(j)
}

func (t *tools) sendable(ctx context.Context, chat string) (types.JID, error) {
	jid, err := t.resolveChat(ctx, chat)
	if err != nil {
		return jid, err
	}
	if !t.cfg.SendAllow.Allows(jid) {
		return jid, fmt.Errorf("sending to %s is not allowed (not in WA_SEND_ALLOW)", jid)
	}
	return jid, nil
}

func (t *tools) readable(ctx context.Context, chat string) (types.JID, error) {
	jid, err := t.resolveChat(ctx, chat)
	if err != nil {
		return jid, err
	}
	if !t.cfg.ReadAllow.Allows(jid) {
		return jid, fmt.Errorf("reading %s is not allowed (not in WA_READ_ALLOW)", jid)
	}
	return jid, nil
}

// ---- tool inputs/outputs ----

type sendIn struct {
	Chat    string `json:"chat" jsonschema:"chat JID (123@g.us or 4477...@s.whatsapp.net), phone number with country code, or exact chat name"`
	Text    string `json:"text" jsonschema:"message text (WhatsApp formatting: *bold* _italic_ ~strike~ and triple backticks)"`
	ReplyTo string `json:"reply_to,omitempty" jsonschema:"optional message ID to quote-reply to"`
}
type sentOut struct {
	MessageID string `json:"message_id"`
	Chat      string `json:"chat"`
}
type fileIn struct {
	Chat     string `json:"chat" jsonschema:"chat JID, phone number or exact chat name"`
	Filename string `json:"filename" jsonschema:"file name with extension; decides image/video/audio/document"`
	Data     string `json:"data_base64" jsonschema:"file contents, base64-encoded (max 16 MB)"`
	Caption  string `json:"caption,omitempty"`
}
type reactIn struct {
	Chat      string `json:"chat"`
	MessageID string `json:"message_id"`
	Emoji     string `json:"emoji" jsonschema:"one emoji; empty string removes the reaction"`
}
type okOut struct {
	OK bool `json:"ok"`
}
type readIn struct {
	Chat   string `json:"chat" jsonschema:"chat JID, phone number or exact chat name"`
	Limit  int    `json:"limit,omitempty" jsonschema:"max messages, default 20, max 200"`
	Since  string `json:"since,omitempty" jsonschema:"only messages after this time (RFC3339, e.g. 2026-10-06T09:00:00Z)"`
	Before string `json:"before,omitempty" jsonschema:"only messages before this time (RFC3339), for paging back"`
}
type messagesOut struct {
	Messages []StoredMessage `json:"messages"`
}
type searchIn struct {
	Query string `json:"query"`
	Chat  string `json:"chat,omitempty" jsonschema:"optional: limit to one chat"`
	Limit int    `json:"limit,omitempty" jsonschema:"default 20, max 200"`
}
type chatsOut struct {
	Chats []ChatInfo `json:"chats"`
}
type groupsOut struct {
	Groups []GroupSummary `json:"groups"`
}
type downloadIn struct {
	Chat      string `json:"chat"`
	MessageID string `json:"message_id"`
}
type statusOut struct {
	WhatsApp        string `json:"whatsapp"`
	Account         string `json:"account,omitempty"`
	SendAllow       string `json:"send_allow"`
	ReadAllow       string `json:"read_allow"`
	SendsLeftInHour int    `json:"sends_left_this_hour"`
	Version         string `json:"version"`
}
type empty struct{}

func clampLimit(n int) int {
	if n <= 0 {
		return 20
	}
	if n > 200 {
		return 200
	}
	return n
}

func parseTime(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, fmt.Errorf("bad time %q: use RFC3339 like 2026-10-06T09:00:00Z", s)
	}
	return t.Unix(), nil
}

func NewMCPServer(t *tools) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "whatsapp-mcp-server", Version: version}, &mcp.ServerOptions{
		Instructions: "WhatsApp access through a linked device. Sending and reading are limited to the chats the owner allow-listed; " +
			"call get_status to see them. Only messages received after linking (plus WhatsApp's initial history sync) can be read. " +
			"Never send to a chat the user did not ask for.",
	})

	mcp.AddTool(s, &mcp.Tool{Name: "get_status", Description: "Connection state, linked account, allow-lists and sends left this hour."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ empty) (*mcp.CallToolResult, statusOut, error) {
			state, _, _ := t.wa.LinkState()
			return nil, statusOut{WhatsApp: state, Account: t.wa.Account(), SendAllow: t.cfg.SendAllow.String(),
				ReadAllow: t.cfg.ReadAllow.String(), SendsLeftInHour: t.rate.Remaining(), Version: version}, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_chats", Description: "Chats this server knows about and may send to or read, newest activity first."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ empty) (*mcp.CallToolResult, chatsOut, error) {
			stored, err := t.st.ListChats(ctx)
			if err != nil {
				return nil, chatsOut{}, err
			}
			seen := map[string]bool{}
			out := chatsOut{Chats: []ChatInfo{}}
			add := func(c ChatInfo) {
				jid, err := types.ParseJID(c.JID)
				if err != nil || seen[c.JID] {
					return
				}
				c.CanSend, c.CanRead = t.cfg.SendAllow.Allows(jid), t.cfg.ReadAllow.Allows(jid)
				if c.CanSend || c.CanRead {
					seen[c.JID] = true
					out.Chats = append(out.Chats, c)
				}
			}
			for _, c := range stored {
				add(c)
			}
			for _, list := range []AllowList{t.cfg.SendAllow, t.cfg.ReadAllow} {
				for j := range list.JIDs {
					add(ChatInfo{JID: j, Name: t.st.ChatName(ctx, j)})
				}
			}
			return nil, out, nil
		})

	if !t.cfg.SendAllow.Empty() {
		mcp.AddTool(s, &mcp.Tool{Name: "send_message", Description: "Send a text message to an allow-listed chat."},
			func(ctx context.Context, _ *mcp.CallToolRequest, in sendIn) (*mcp.CallToolResult, sentOut, error) {
				if strings.TrimSpace(in.Text) == "" || len(in.Text) > 65000 {
					return nil, sentOut{}, errors.New("text must be 1 to 65,000 characters")
				}
				jid, err := t.sendable(ctx, in.Chat)
				if err != nil {
					return nil, sentOut{}, err
				}
				if err := t.wa.ready(); err != nil {
					return nil, sentOut{}, err
				}
				if err := t.rate.Take(); err != nil {
					return nil, sentOut{}, err
				}
				id, err := t.wa.SendText(ctx, jid, in.Text, in.ReplyTo)
				if err != nil {
					return nil, sentOut{}, err
				}
				t.wa.log.Info("sent message", "chat", jid.String(), "chars", len(in.Text), "id", id)
				return nil, sentOut{MessageID: id, Chat: jid.String()}, nil
			})

		mcp.AddTool(s, &mcp.Tool{Name: "send_file", Description: "Send an image, video, audio file or document (base64) to an allow-listed chat."},
			func(ctx context.Context, _ *mcp.CallToolRequest, in fileIn) (*mcp.CallToolResult, sentOut, error) {
				data, err := base64.StdEncoding.DecodeString(in.Data)
				if err != nil {
					return nil, sentOut{}, fmt.Errorf("data_base64 is not valid base64: %w", err)
				}
				if len(data) == 0 || len(data) > 16<<20 {
					return nil, sentOut{}, errors.New("file must be between 1 byte and 16 MB")
				}
				if in.Filename == "" {
					return nil, sentOut{}, errors.New("filename is required")
				}
				jid, err := t.sendable(ctx, in.Chat)
				if err != nil {
					return nil, sentOut{}, err
				}
				if err := t.wa.ready(); err != nil {
					return nil, sentOut{}, err
				}
				if err := t.rate.Take(); err != nil {
					return nil, sentOut{}, err
				}
				id, err := t.wa.SendFile(ctx, jid, data, in.Filename, in.Caption)
				if err != nil {
					return nil, sentOut{}, err
				}
				t.wa.log.Info("sent file", "chat", jid.String(), "bytes", len(data), "id", id)
				return nil, sentOut{MessageID: id, Chat: jid.String()}, nil
			})
	}

	if !t.cfg.SendAllow.Empty() && !t.cfg.ReadAllow.Empty() {
		mcp.AddTool(s, &mcp.Tool{Name: "send_reaction", Description: "React with an emoji to a stored message in a chat that is allow-listed for both sending and reading."},
			func(ctx context.Context, _ *mcp.CallToolRequest, in reactIn) (*mcp.CallToolResult, okOut, error) {
				jid, err := t.sendable(ctx, in.Chat)
				if err != nil {
					return nil, okOut{}, err
				}
				if !t.cfg.ReadAllow.Allows(jid) {
					return nil, okOut{}, errors.New("reactions need the chat in WA_READ_ALLOW too")
				}
				if err := t.wa.ready(); err != nil {
					return nil, okOut{}, err
				}
				if err := t.rate.Take(); err != nil {
					return nil, okOut{}, err
				}
				return nil, okOut{OK: true}, t.wa.React(ctx, jid, in.MessageID, in.Emoji)
			})
	}

	if !t.cfg.ReadAllow.Empty() {
		mcp.AddTool(s, &mcp.Tool{Name: "read_messages", Description: "Read recent messages from an allow-listed chat, newest first."},
			func(ctx context.Context, _ *mcp.CallToolRequest, in readIn) (*mcp.CallToolResult, messagesOut, error) {
				jid, err := t.readable(ctx, in.Chat)
				if err != nil {
					return nil, messagesOut{}, err
				}
				after, err := parseTime(in.Since)
				if err != nil {
					return nil, messagesOut{}, err
				}
				before, err := parseTime(in.Before)
				if err != nil {
					return nil, messagesOut{}, err
				}
				msgs, err := t.st.Messages(ctx, jid.String(), clampLimit(in.Limit), after, before)
				if msgs == nil {
					msgs = []StoredMessage{}
				}
				return nil, messagesOut{Messages: msgs}, err
			})

		mcp.AddTool(s, &mcp.Tool{Name: "search_messages", Description: "Search stored messages by text across allow-listed chats."},
			func(ctx context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, messagesOut, error) {
				if strings.TrimSpace(in.Query) == "" {
					return nil, messagesOut{}, errors.New("query is required")
				}
				var chats []string
				if in.Chat != "" {
					jid, err := t.readable(ctx, in.Chat)
					if err != nil {
						return nil, messagesOut{}, err
					}
					chats = []string{jid.String()}
				} else {
					all, err := t.st.ListChats(ctx)
					if err != nil {
						return nil, messagesOut{}, err
					}
					for _, c := range all {
						if jid, err := types.ParseJID(c.JID); err == nil && t.cfg.ReadAllow.Allows(jid) {
							chats = append(chats, c.JID)
						}
					}
				}
				msgs, err := t.st.Search(ctx, in.Query, chats, clampLimit(in.Limit))
				if msgs == nil {
					msgs = []StoredMessage{}
				}
				return nil, messagesOut{Messages: msgs}, err
			})

		mcp.AddTool(s, &mcp.Tool{Name: "download_media", Description: "Download the image, voice note, video or document attached to a stored message (max 10 MB)."},
			func(ctx context.Context, _ *mcp.CallToolRequest, in downloadIn) (*mcp.CallToolResult, any, error) {
				jid, err := t.readable(ctx, in.Chat)
				if err != nil {
					return nil, nil, err
				}
				data, m, mimeType, err := t.wa.Download(ctx, jid, in.MessageID)
				if err != nil {
					return nil, nil, err
				}
				if len(data) > 10<<20 {
					return nil, nil, fmt.Errorf("media is %d MB; the limit is 10 MB", len(data)>>20)
				}
				if strings.HasPrefix(mimeType, "image/") {
					return &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{Data: data, MIMEType: mimeType}}}, nil, nil
				}
				name := m.Filename
				if name == "" {
					name = m.ID
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{
					URI: "whatsapp://" + jid.String() + "/" + m.ID + "/" + name, MIMEType: mimeType, Blob: data}}}}, nil, nil
			})
	}

	if t.cfg.Discovery {
		mcp.AddTool(s, &mcp.Tool{Name: "list_groups", Description: "Every group the linked account is in, with JIDs and whether each is allow-listed."},
			func(ctx context.Context, _ *mcp.CallToolRequest, _ empty) (*mcp.CallToolResult, groupsOut, error) {
				g, err := t.wa.Groups(ctx)
				if g == nil {
					g = []GroupSummary{}
				}
				return nil, groupsOut{Groups: g}, err
			})
	}
	return s
}
