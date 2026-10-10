package main

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"go.mau.fi/whatsmeow/types"
)

// Config is read once from environment variables at startup.
type Config struct {
	PublicURL     string // e.g. https://wa.example.com (no trailing slash)
	Listen        string // e.g. :8080
	DatabaseURL   string // postgres://... or a SQLite file path
	AdminPassword string // unlocks the OAuth consent page and /admin
	APIToken      string // optional static bearer token (Claude Code, scripts)

	SendAllow AllowList // chats the server may send to
	ReadAllow AllowList // chats whose messages are stored and readable

	SendPerHour   int           // global send rate limit
	PairPhone     string        // optional: link with a pairing code instead of QR
	DeviceName    string        // shown in WhatsApp > Linked devices
	RedirectHosts []string      // hosts allowed as OAuth redirect targets
	Discovery     bool          // expose list_groups (all groups the account is in)
	Trigger       TriggerConfig // optional webhook when someone writes and nobody replies
	LogLevel      string
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func LoadConfig() (*Config, error) {
	c := &Config{
		PublicURL:     strings.TrimRight(env("PUBLIC_URL", ""), "/"),
		Listen:        env("LISTEN_ADDR", ":8080"),
		DatabaseURL:   env("DATABASE_URL", "/data/whatsapp.db"),
		AdminPassword: os.Getenv("ADMIN_PASSWORD"),
		APIToken:      os.Getenv("API_TOKEN"),
		PairPhone:     digitsOnly(env("WA_PAIR_PHONE", "")),
		DeviceName:    env("WA_DEVICE_NAME", "WhatsApp MCP"),
		Discovery:     env("WA_DISCOVERY", "false") == "true",
		LogLevel:      env("LOG_LEVEL", "INFO"),
	}
	var err error
	if c.SendAllow, err = ParseAllowList(os.Getenv("WA_SEND_ALLOW")); err != nil {
		return nil, fmt.Errorf("WA_SEND_ALLOW: %w", err)
	}
	if c.ReadAllow, err = ParseAllowList(os.Getenv("WA_READ_ALLOW")); err != nil {
		return nil, fmt.Errorf("WA_READ_ALLOW: %w", err)
	}
	if c.SendPerHour, err = strconv.Atoi(env("WA_SEND_PER_HOUR", "60")); err != nil || c.SendPerHour < 1 {
		return nil, fmt.Errorf("WA_SEND_PER_HOUR must be a positive number")
	}
	for _, h := range strings.Split(env("OAUTH_REDIRECT_HOSTS", "claude.ai,claude.com,localhost,127.0.0.1"), ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			c.RedirectHosts = append(c.RedirectHosts, h)
		}
	}

	u, err := url.Parse(c.PublicURL)
	if c.PublicURL == "" || err != nil || u.Host == "" {
		return nil, fmt.Errorf("PUBLIC_URL is required, e.g. https://wa.example.com")
	}
	if u.Scheme != "https" && !isLoopbackHost(u.Hostname()) {
		return nil, fmt.Errorf("PUBLIC_URL must use https (http is only allowed for localhost)")
	}
	if len(c.AdminPassword) < 12 {
		return nil, fmt.Errorf("ADMIN_PASSWORD is required and must be at least 12 characters")
	}
	if c.APIToken != "" && len(c.APIToken) < 32 {
		return nil, fmt.Errorf("API_TOKEN must be at least 32 characters (try: openssl rand -hex 32)")
	}
	if err := loadTriggerConfig(c); err != nil {
		return nil, err
	}
	return c, nil
}

// AllowList is a set of chat JIDs, or "*" for every chat.
type AllowList struct {
	All  bool
	JIDs map[string]bool
}

// ParseAllowList accepts a comma-separated list of JIDs (123@g.us,
// 4477...@s.whatsapp.net), bare phone numbers, or "*".
func ParseAllowList(s string) (AllowList, error) {
	a := AllowList{JIDs: map[string]bool{}}
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		switch {
		case item == "":
		case item == "*":
			a.All = true
		case strings.Contains(item, "@"):
			jid, err := types.ParseJID(item)
			if err != nil {
				return a, fmt.Errorf("bad JID %q: %w", item, err)
			}
			a.JIDs[jid.ToNonAD().String()] = true
		default:
			d := digitsOnly(item)
			if len(d) < 7 {
				return a, fmt.Errorf("bad entry %q: use a JID or a phone number with country code", item)
			}
			a.JIDs[types.NewJID(d, types.DefaultUserServer).String()] = true
		}
	}
	return a, nil
}

func (a AllowList) Empty() bool { return !a.All && len(a.JIDs) == 0 }

func (a AllowList) Allows(jid types.JID) bool {
	return a.All || a.JIDs[jid.ToNonAD().String()]
}

func (a AllowList) String() string {
	if a.All {
		return "*"
	}
	if len(a.JIDs) == 0 {
		return "(none)"
	}
	out := make([]string, 0, len(a.JIDs))
	for j := range a.JIDs {
		out = append(out, j)
	}
	return strings.Join(out, ",")
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return strings.TrimPrefix(b.String(), "00")
}

func isLoopbackHost(h string) bool {
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}
