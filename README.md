# whatsapp-mcp-server

A self-hosted WhatsApp server for AI assistants. It links to a WhatsApp account as a **linked device** (like WhatsApp Web) and exposes it over the **Model Context Protocol** on a public HTTPS URL, so Claude (claude.ai, Claude Desktop, Claude Code) or any MCP client can send and read messages.

What makes it different from local WhatsApp MCP servers:

- **Remote, not local.** It runs on a server you own, so it works from cloud agents and phones, not only your laptop.
- **Built-in OAuth.** Dynamic client registration, PKCE and refresh tokens, so it plugs into claude.ai's "Add custom connector" with no extra auth service.
- **Locked down by default.** Nothing can be sent or read until you allow-list chats. No "send to anyone" unless you explicitly set `*`.
- **One container.** Docker, Coolify, or any host. Postgres or SQLite.

> ⚠️ **Read this first.** This uses [whatsmeow](https://github.com/tulir/whatsmeow), an unofficial WhatsApp client. Using unofficial clients is against WhatsApp's Terms of Service and **accounts can be banned**. Use a spare number, never your main one. Keep volume low, don't message strangers, and don't send bulk messages.

## Tools

| Tool | Needs | What it does |
|---|---|---|
| `get_status` | — | Connection state, linked account, allow-lists, sends left this hour |
| `list_chats` | — | Allow-listed chats with names and last activity |
| `send_message` | `WA_SEND_ALLOW` | Send text, optionally as a quoted reply |
| `send_file` | `WA_SEND_ALLOW` | Send an image, video, audio file or document (base64) |
| `send_reaction` | both lists | React to a stored message |
| `read_messages` | `WA_READ_ALLOW` | Recent messages in a chat, with time paging |
| `search_messages` | `WA_READ_ALLOW` | Text search across allow-listed chats |
| `download_media` | `WA_READ_ALLOW` | Fetch a photo, voice note, video or document from a message |
| `list_groups` | `WA_DISCOVERY=true` | Every group the account is in, with JIDs |

Tools whose allow-list is empty are not registered at all. Reading only covers messages received after linking, plus whatever WhatsApp sends in its initial history sync, and only for chats in `WA_READ_ALLOW`. Other chats are never stored.

## Quick start (Docker Compose)

```bash
git clone https://github.com/ayyazzafar/whatsapp-mcp-server && cd whatsapp-mcp-server
cp .env.example .env    # set PUBLIC_URL and ADMIN_PASSWORD at least
docker compose up -d --build
```

Put a TLS reverse proxy (Caddy, Traefik, nginx) in front of port 8080. `PUBLIC_URL` must be the HTTPS address it serves.

## Deploy on Coolify

1. **Database (recommended):** create a Postgres database for it, or use an existing Postgres with a new database and user. The WhatsApp session lives there, so it survives redeploys and is covered by your database backups. Without `DATABASE_URL` it uses SQLite at `/data/whatsapp.db`, and you must add a persistent volume at `/data`.
2. **New resource → Application →** this repo, build pack **Dockerfile**, port **8080**.
3. **Domain:** e.g. `https://wa.example.com`.
4. **Environment variables:** `PUBLIC_URL`, `ADMIN_PASSWORD`, `DATABASE_URL`, `WA_SEND_ALLOW`, `WA_READ_ALLOW` (see `.env.example`).
5. **Deploy**, then link the phone (below).

## Link your WhatsApp

Open `https://<your-domain>/admin` (any username, your `ADMIN_PASSWORD`). On the phone: **WhatsApp → Settings → Linked devices → Link a device** and scan the QR. The QR also prints in the container logs. If you'd rather type a code, set `WA_PAIR_PHONE` to the number with country code and choose **Link with phone number instead** on the phone.

After linking, `/admin` lists your groups with their JIDs, so you can fill in the allow-lists and redeploy.

## Connect Claude

**claude.ai / Claude Desktop:** Settings → Connectors → Add custom connector

- URL: `https://<your-domain>/mcp`
- Authentication: **Sign in now**, OAuth client: **Register automatically**
- Claude opens a consent page. Enter `ADMIN_PASSWORD` and click **Allow**.

**Claude Code** (static token): set `API_TOKEN`, then

```bash
claude mcp add --transport http whatsapp https://<your-domain>/mcp --header "Authorization: Bearer $API_TOKEN"
```

## Configuration

| Variable | Default | Notes |
|---|---|---|
| `PUBLIC_URL` | — | **Required.** HTTPS base URL, no trailing slash |
| `ADMIN_PASSWORD` | — | **Required**, 12+ characters. Unlocks OAuth consent and `/admin` |
| `DATABASE_URL` | `/data/whatsapp.db` | `postgres://…` or a SQLite path |
| `WA_SEND_ALLOW` | empty | Group JIDs, phone numbers, or `*` |
| `WA_READ_ALLOW` | empty | Same format. Only these chats are stored |
| `WA_SEND_PER_HOUR` | `60` | Global cap across all send tools |
| `WA_PAIR_PHONE` | — | Link with a pairing code instead of a QR |
| `WA_DISCOVERY` | `false` | Expose `list_groups` |
| `WA_DEVICE_NAME` | `WhatsApp MCP` | Shown in Linked devices |
| `API_TOKEN` | — | Optional static bearer token, 32+ characters |
| `OAUTH_REDIRECT_HOSTS` | `claude.ai,claude.com,localhost,127.0.0.1` | Hosts OAuth clients may redirect to |
| `LISTEN_ADDR` | `:8080` | |
| `LOG_LEVEL` | `INFO` | `DEBUG` for whatsmeow detail |

## Security model

- Every MCP call needs a bearer token: an OAuth access token (1 hour, refresh tokens rotate and last 30 days) or `API_TOKEN`. Only SHA-256 hashes of tokens are stored.
- OAuth clients may only redirect to `OAUTH_REDIRECT_HOSTS`. PKCE (S256) is mandatory, and codes are single-use and expire after 5 minutes.
- The consent page and `/admin` need `ADMIN_PASSWORD`. After 5 wrong passwords in 15 minutes, logins pause for 15 minutes.
- Allow-lists are enforced in the server, not left to the model's judgement.
- Sends are rate-limited and logged (chat, size, message ID; never the text).
- The container runs as a non-root user on a distroless image.
- Anyone with your `ADMIN_PASSWORD` can approve a client. Use a long random one.

## Development

```bash
go test ./...
PUBLIC_URL=http://localhost:8080 ADMIN_PASSWORD=change-me-please DATABASE_URL=./dev.db go run .
```

## License

MIT
