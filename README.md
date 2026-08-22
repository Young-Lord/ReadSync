# ReadSync

A self-hosted browsing history synchronization tool. It consists of a Go backend server (with embedded Web UI) and a Tampermonkey/Greasemonkey userscript that automatically records every page you visit.

## Features

- Automatic browsing history recording via browser userscript
- SPA navigation support (pushState / replaceState / popstate)
- Live page title tracking: title changes are synced and refresh the current entry (visible via auto-refresh)
- Deduplication: same URL within a configurable time window is not recorded again
- Full-text search on URL and title
- Paginated browsing history with auto-refresh polling
- Dark mode support (follows system preference)
- Single binary deployment, no CGO required

## Quick Start

### 1. Configure

Copy the example config and edit it:

```bash
cp config.example.json config.json
```

```json
{
  "username": "your_username",
  "password": "your_password",
  "port": 8080,
  "db_path": "data.db",
  "base_url": "",
  "host": "http://localhost:8080",
  "max_entries": 100000,
  "dedupe_minutes": 10,
  "poll_interval_ms": 30000
}
```

| Field | Description | Default |
|---|---|---|
| `username` | HTTP Basic Auth username | (required) |
| `password` | HTTP Basic Auth password | (required) |
| `port` | Server listen port | `8080` |
| `db_path` | SQLite database file path | `data.db` |
| `base_url` | URL path prefix (e.g. `/readsync`) for reverse proxy | `""` |
| `host` | Public address used by browsers, the **only** source for the generated userscript's `@connect` and `SERVER` (e.g. `https://read.example.com` or `read.example.com:8443`) | (required) |
| `max_entries` | Maximum number of entries to keep | `100000` |
| `dedupe_minutes` | Time window (in minutes) for deduplication | `10` |
| `poll_interval_ms` | Web UI polling interval in milliseconds | `30000` |

### 2. Build & Run

```bash
go build -o readsync .
./readsync
```

Or download a pre-built binary from [Releases](https://github.com/Young-Lord/ReadSync/releases).

### 3. Install the Userscript

1. Install [Tampermonkey](https://www.tampermonkey.net/) or [Greasemonkey](https://www.greasespot.net/) in your browser.
2. Open the Web UI and log in.
3. Click **安装脚本** (Install Script) in the header and confirm the installation in your userscript manager.

The script is generated on the fly from `webui/userscript.template.js` with your credentials and the configured `host`, so no manual editing is required.

### 4. Access the Web UI

Open `http://localhost:8080/` (or your configured address) in your browser and log in with your credentials.

## Reverse Proxy

If you run ReadSync behind a reverse proxy (e.g. Nginx), set `base_url` in your config to the path prefix:

```json
{
  "base_url": "/readsync"
}
```

Then proxy requests to `http://localhost:8080/readsync/`.

When the reverse proxy rewrites the `Host` header (or does not forward `X-Forwarded-Proto`), the generated userscript may point at the internal address. `host` must always be set explicitly to the public address used by browsers — it is the **only** source of the server address in the generated userscript:

```json
{
  "base_url": "/readsync",
  "host": "https://read.example.com"
}
```

`host` accepts `host`, `host:port`, or `scheme://host:port` formats. The server refuses to start without it. It only affects the generated userscript (`@connect` and `SERVER`); the web UI and API always work through whatever address you use to reach the server.

## API

All endpoints require HTTP Basic Auth.

| Method | Path | Description |
|---|---|---|
| `POST` | `/api/v1/entry` | Create a new entry (`{"url": "...", "title": "..."}`) |
| `PATCH` | `/api/v1/entry` | Report a title change (`{"url": "...", "title": "..."}`); if the latest entry has this URL its title is updated and `latest-id` advances (so the UI refreshes), otherwise a new entry is inserted |
| `GET` | `/api/v1/entry?page=1&per_page=20&q=keyword` | List entries (paginated, searchable) |
| `DELETE` | `/api/v1/entry/{id}` | Delete an entry by ID |
| `GET` | `/api/v1/entry/latest-id` | Get the latest entry ID (used for polling) |

## License

MIT
