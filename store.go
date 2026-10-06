package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

// OpenDB opens Postgres (postgres://...) or SQLite (anything else) and
// returns the handle plus the whatsmeow dialect name.
func OpenDB(dsn string) (*sql.DB, string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			return nil, "", err
		}
		db.SetMaxOpenConns(10)
		return db, "postgres", db.Ping()
	}
	path := strings.TrimPrefix(dsn, "file:")
	if i := strings.Index(path, "?"); i >= 0 {
		path = path[:i]
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)")
	if err != nil {
		return nil, "", err
	}
	return db, "sqlite3", db.Ping()
}

// Store holds this server's own tables (messages, chats, OAuth). The
// WhatsApp session itself lives in whatsmeow's tables in the same database.
type Store struct {
	db      *sql.DB
	dialect string
}

type StoredMessage struct {
	ChatJID    string `json:"chat"`
	ID         string `json:"id"`
	SenderJID  string `json:"sender"`
	SenderName string `json:"sender_name,omitempty"`
	Time       string `json:"time"`
	FromMe     bool   `json:"from_me"`
	Text       string `json:"text,omitempty"`
	MediaType  string `json:"media_type,omitempty"`
	Filename   string `json:"filename,omitempty"`
	ts         int64
	raw        []byte
}

type ChatInfo struct {
	JID      string `json:"jid"`
	Name     string `json:"name,omitempty"`
	LastTime string `json:"last_message_time,omitempty"`
	CanSend  bool   `json:"can_send"`
	CanRead  bool   `json:"can_read"`
}

func NewStore(ctx context.Context, db *sql.DB, dialect string) (*Store, error) {
	blob := "BLOB"
	if dialect == "postgres" {
		blob = "BYTEA"
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS wamcp_messages (
			chat_jid TEXT NOT NULL, id TEXT NOT NULL, sender_jid TEXT NOT NULL,
			sender_name TEXT NOT NULL DEFAULT '', ts BIGINT NOT NULL, from_me INTEGER NOT NULL,
			text TEXT NOT NULL DEFAULT '', media_type TEXT NOT NULL DEFAULT '',
			filename TEXT NOT NULL DEFAULT '', raw ` + blob + `,
			PRIMARY KEY (chat_jid, id))`,
		`CREATE INDEX IF NOT EXISTS wamcp_messages_chat_ts ON wamcp_messages (chat_jid, ts)`,
		`CREATE TABLE IF NOT EXISTS wamcp_chats (jid TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '', last_ts BIGINT NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS wamcp_oauth_clients (client_id TEXT PRIMARY KEY, name TEXT NOT NULL, redirect_uris TEXT NOT NULL, created BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS wamcp_oauth_codes (code_hash TEXT PRIMARY KEY, client_id TEXT NOT NULL, redirect_uri TEXT NOT NULL, challenge TEXT NOT NULL, resource TEXT NOT NULL, expires BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS wamcp_oauth_tokens (token_hash TEXT PRIMARY KEY, kind TEXT NOT NULL, client_id TEXT NOT NULL, expires BIGINT NOT NULL)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return nil, fmt.Errorf("create tables: %w", err)
		}
	}
	return &Store{db: db, dialect: dialect}, nil
}

func (s *Store) SaveMessage(ctx context.Context, m *StoredMessage) error {
	fromMe := 0
	if m.FromMe {
		fromMe = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO wamcp_messages
		(chat_jid, id, sender_jid, sender_name, ts, from_me, text, media_type, filename, raw)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT (chat_jid, id) DO NOTHING`,
		m.ChatJID, m.ID, m.SenderJID, m.SenderName, m.ts, fromMe, m.Text, m.MediaType, m.Filename, m.raw)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO wamcp_chats (jid, last_ts) VALUES ($1,$2)
		ON CONFLICT (jid) DO UPDATE SET last_ts = CASE WHEN excluded.last_ts > wamcp_chats.last_ts THEN excluded.last_ts ELSE wamcp_chats.last_ts END`,
		m.ChatJID, m.ts)
	return err
}

func (s *Store) SetChatName(ctx context.Context, jid, name string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO wamcp_chats (jid, name) VALUES ($1,$2)
		ON CONFLICT (jid) DO UPDATE SET name = excluded.name`, jid, name)
	return err
}

func (s *Store) ChatName(ctx context.Context, jid string) string {
	var name string
	_ = s.db.QueryRowContext(ctx, `SELECT name FROM wamcp_chats WHERE jid=$1`, jid).Scan(&name)
	return name
}

// FindChatByName returns the JID of a stored chat whose name matches exactly
// (case-insensitive), or "" if none or several match.
func (s *Store) FindChatByName(ctx context.Context, name string) (string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT jid FROM wamcp_chats WHERE LOWER(name)=LOWER($1)`, name)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var j string
		if err := rows.Scan(&j); err != nil {
			return "", err
		}
		found = append(found, j)
	}
	if len(found) > 1 {
		return "", fmt.Errorf("%d chats are named %q; use the JID instead", len(found), name)
	}
	if len(found) == 0 {
		return "", nil
	}
	return found[0], rows.Err()
}

func (s *Store) ListChats(ctx context.Context) ([]ChatInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT jid, name, last_ts FROM wamcp_chats ORDER BY last_ts DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatInfo
	for rows.Next() {
		var c ChatInfo
		var ts int64
		if err := rows.Scan(&c.JID, &c.Name, &ts); err != nil {
			return nil, err
		}
		if ts > 0 {
			c.LastTime = time.Unix(ts, 0).UTC().Format(time.RFC3339)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

const msgCols = `chat_jid, id, sender_jid, sender_name, ts, from_me, text, media_type, filename`

func scanMessages(rows *sql.Rows) ([]StoredMessage, error) {
	defer rows.Close()
	var out []StoredMessage
	for rows.Next() {
		var m StoredMessage
		var fromMe int
		if err := rows.Scan(&m.ChatJID, &m.ID, &m.SenderJID, &m.SenderName, &m.ts, &fromMe, &m.Text, &m.MediaType, &m.Filename); err != nil {
			return nil, err
		}
		m.FromMe = fromMe == 1
		m.Time = time.Unix(m.ts, 0).UTC().Format(time.RFC3339)
		out = append(out, m)
	}
	return out, rows.Err()
}

// Messages returns up to limit messages in a chat, newest first, optionally
// bounded by unix timestamps (0 = unbounded).
func (s *Store) Messages(ctx context.Context, chat string, limit int, after, before int64) ([]StoredMessage, error) {
	if before == 0 {
		before = 1 << 62
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+msgCols+` FROM wamcp_messages
		WHERE chat_jid=$1 AND ts>$2 AND ts<$3 ORDER BY ts DESC LIMIT $4`, chat, after, before, limit)
	if err != nil {
		return nil, err
	}
	return scanMessages(rows)
}

// Search finds messages containing query (case-insensitive) in the given chats.
func (s *Store) Search(ctx context.Context, query string, chats []string, limit int) ([]StoredMessage, error) {
	if len(chats) == 0 {
		return nil, nil
	}
	args := []any{"%" + strings.ToLower(query) + "%", limit}
	ph := make([]string, len(chats))
	for i, c := range chats {
		args = append(args, c)
		ph[i] = fmt.Sprintf("$%d", i+3)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+msgCols+` FROM wamcp_messages
		WHERE LOWER(text) LIKE $1 AND chat_jid IN (`+strings.Join(ph, ",")+`) ORDER BY ts DESC LIMIT $2`, args...)
	if err != nil {
		return nil, err
	}
	return scanMessages(rows)
}

func (s *Store) GetMessage(ctx context.Context, chat, id string) (*StoredMessage, error) {
	var m StoredMessage
	var fromMe int
	err := s.db.QueryRowContext(ctx, `SELECT `+msgCols+`, raw FROM wamcp_messages WHERE chat_jid=$1 AND id=$2`, chat, id).
		Scan(&m.ChatJID, &m.ID, &m.SenderJID, &m.SenderName, &m.ts, &fromMe, &m.Text, &m.MediaType, &m.Filename, &m.raw)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("message %s not found in %s (only messages received since linking are stored)", id, chat)
	}
	m.FromMe = fromMe == 1
	return &m, err
}

// ---- OAuth storage ----

func (s *Store) AddClient(ctx context.Context, id, name, redirects string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO wamcp_oauth_clients (client_id, name, redirect_uris, created) VALUES ($1,$2,$3,$4)`,
		id, name, redirects, time.Now().Unix())
	return err
}

func (s *Store) GetClient(ctx context.Context, id string) (name, redirects string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT name, redirect_uris FROM wamcp_oauth_clients WHERE client_id=$1`, id).Scan(&name, &redirects)
	return
}

func (s *Store) CountClients(ctx context.Context) (n int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM wamcp_oauth_clients`).Scan(&n)
	return
}

func (s *Store) SaveCode(ctx context.Context, hash, client, redirect, challenge, resource string, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO wamcp_oauth_codes (code_hash, client_id, redirect_uri, challenge, resource, expires) VALUES ($1,$2,$3,$4,$5,$6)`,
		hash, client, redirect, challenge, resource, expires.Unix())
	return err
}

// TakeCode deletes and returns an authorization code (codes are single-use).
func (s *Store) TakeCode(ctx context.Context, hash string) (client, redirect, challenge, resource string, expires int64, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT client_id, redirect_uri, challenge, resource, expires FROM wamcp_oauth_codes WHERE code_hash=$1`, hash).
		Scan(&client, &redirect, &challenge, &resource, &expires)
	if err != nil {
		return
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM wamcp_oauth_codes WHERE code_hash=$1`, hash)
	if err == nil {
		if n, _ := res.RowsAffected(); n != 1 {
			err = sql.ErrNoRows // lost a race with a concurrent redemption
		}
	}
	return
}

func (s *Store) SaveToken(ctx context.Context, hash, kind, client string, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO wamcp_oauth_tokens (token_hash, kind, client_id, expires) VALUES ($1,$2,$3,$4)`,
		hash, kind, client, expires.Unix())
	return err
}

func (s *Store) CheckToken(ctx context.Context, hash, kind string) (client string, ok bool) {
	var expires int64
	err := s.db.QueryRowContext(ctx, `SELECT client_id, expires FROM wamcp_oauth_tokens WHERE token_hash=$1 AND kind=$2`, hash, kind).Scan(&client, &expires)
	return client, err == nil && expires > time.Now().Unix()
}

func (s *Store) DeleteToken(ctx context.Context, hash string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM wamcp_oauth_tokens WHERE token_hash=$1`, hash)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *Store) PurgeExpired(ctx context.Context) {
	now := time.Now().Unix()
	_, _ = s.db.ExecContext(ctx, `DELETE FROM wamcp_oauth_codes WHERE expires < $1`, now)
	_, _ = s.db.ExecContext(ctx, `DELETE FROM wamcp_oauth_tokens WHERE expires < $1`, now)
}
