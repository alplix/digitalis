<div align="center">

<img src="https://raw.githubusercontent.com/alplix/digitalis/main/docs/logo.svg" alt="Digitalis" width="160"/>

# 🌸 Digitalis

> The **Yüksük Otu** — a blazing-fast BitTorrent client written **from scratch** in Go,
> with a gorgeous multilingual **digital-forest-themed** web UI.

[![Go Version](https://img.shields.io/badge/Go-1.23+-2b1322)](https://golang.org)
[![License](https://img.shields.io/github/license/alplix/digitalis?color=713955)](LICENSE)
[![Built With: Go](https://img.shields.io/badge/Built%20With-Go-5c2f49?logo=go&logoColor=white)](https://golang.org)
[![Zero Deps](https://img.shields.io/badge/Zero%20External-Deps%20(engine)-4a2740)](#-design-philosophy)

*Zero torrent libraries. Zero bloat. One beautiful binary.*

</div>

---

## 🌿 What is Digitalis?

**Digitalis** (Latin for *foxglove*, Türkçe: **Yüksük Otu**) is a self-contained
BitTorrent client built entirely from scratch in Go. No third-party torrent engine,
no Docker, no database — just a single compiled binary that speaks the BitTorrent
protocol natively and serves a beautiful, **multilingual** web dashboard.

Seed your favorite ISOs, share Linux releases, or race private torrents — Digitalis
does it all with a flower on its lapel. 🌺

---

## ✨ Features

| | Feature | Details |
|---|---------|---------|
| 🔄 | **Native protocol** | Tracker announce (HTTP & UDP), peer-wire protocol, bitfield — all hand-written |
| 🧲 | **Magnet metadata** | BEP-9 `ut_metadata` over BEP-10 extensions — magnets resolve to full metadata directly from peers |
| 📡 | **DHT + PEX** | Embedded distributed hash table node (`get_peers`/`announce`) + peer exchange between connected peers |
| 🔁 | **Torrent persistence** | Every torrent is recorded and restored automatically across restarts (`torrents.json`) |
| 🔔 | **Notifications** | WebSocket push on download complete & metadata fetched, shown as toasts in the UI |
| 🖇 | **Bulk actions** | Multi-select pause / resume / delete across torrents |
| ⏱ | **Per-torrent limit** | Individual download cap per torrent, set from the detail panel |
| 🎯 | **Rare-first picking** | Smart piece selection for faster, fairer downloads |
| 🌐 | **Multilingual UI** | **12 languages** with a live switcher: EN, TR, DE, FR, ES, IT, PT, RU, JA, ZH, AR, HI |
| 🗂️ | **Categories** | Nested folders (`unix/linux/debian`), created on the fly, save dir per torrent, drag-free moving |
| 🔌 | **Flexible input** | Raw `.torrent`, magnet URI, or file upload — paste in bulk |
| 📈 | **Dashboard stats** | Hourly / daily / monthly / yearly traffic charts + per-torrent counters |
| 📡 | **Live chart** | Per-second traffic via a built-in WebSocket feed (`/ws`) — no polling jitter |
| 🔍 | **Torrent detail** | Drawer with piece map, per-file progress, health, trackers, comment/creator, rate limit |
| 🧹 | **Search & filter** | Instant search, state/category filters and name/size/progress/speed sorting |
| 📖 | **Guide view** | In-app rehber/guide covering magnets, settings, persistence and discovery |
| 🌗 | **Theme toggle** | Digital-forest dark mode *and* a light variant, remembered in `localStorage` |
| 🌍 | **Country log** | Every peer connection geolocated via an embedded IP→country table (no API calls) |
| 🔗 | **Custom trackers** | Add/remove trackers per torrent over HTTP, UDP or WebSocket from the UI |
| 📁 | **Watch folder** | Drop `.torrent` files in a directory — auto-added & seeded |
| 🚀 | **Speed control** | Global download & upload rate limits, plus a daily upload cap (auto-pause at midnight-reset) |
| 💾 | **Reboot-proof** | Settings, statistics & torrents persist to disk; ships with a ready-made `systemd` unit |
| 🎨 | **Themer's dream** | Deep forest-green "digital woods" theme, with light/dark toggle |

---

## 🖼️ Screenshot

```
┌──────────────────────────────────────────────────────────────┐
│  🌸 Digitalis  [Choose language ▾] [No Docker] [Port 51413] │
│  ┌───────────┐ ┌───────────┐ ┌───────────┐ ┌──────────────┐ │
│  │ Total  3  │ │ Down  1   │ │ Seed  2   │ │ Upload 1.2GB │ │
│  └───────────┘ └───────────┘ └───────────┘ └──────────────┘ │
│  Add torrent:  magnet:?xt=urn:btih:...          [ Add ]     │
│  ┌────────────────────────────────────────────────────────┐ │
│  │ ▸ Ubuntu 24.04 Desktop          [downloading] 45% ▓▓  │ │
│  │   ↓ 1.2 MB/s   ↑ 0 B/s   Peer: 12   Seed/Leech 300/22  │ │
│  └────────────────────────────────────────────────────────┘ │
└──────────────────────────────────────────────────────────────┘
```

---

## 📦 Installation

Only dependency for building: **Go ≥ 1.23**.

```bash
git clone https://github.com/alplix/digitalis.git
cd digitalis
go build -o digitalis ./cmd/digitalis
```

---

## 🚀 Running

```bash
./digitalis \
  --web :1919 \                    # web UI address
  --peer-port 51413 \              # incoming peer port
  --dir /mnt/torrents/downloads \  # base save directory (categories = folders under it)
  --watch /mnt/torrents/watch \    # auto-add folder (optional)
  --download-limit 0 \             # bytes/sec, 0 = unlimited
  --upload-limit 0                 # bytes/sec, 0 = unlimited
```

Then open the dashboard: **`http://YOUR-SERVER:1919/`** 🎉

> 🔓 Open port `51413` on your router for inbound peer connections (better seeding).

---

## ⚙️ systemd (boot-persistent)

```ini
[Unit]
Description=Digitalis BitTorrent Client
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=alp
WorkingDirectory=/home/alp/digitalis
ExecStart=/home/alp/digitalis/digitalis --web :1919 --peer-port 51413 \
          --dir /mnt/torrents/downloads --watch /mnt/torrents/watch
Restart=on-failure
RestartSec=3
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
```

```bash
sudo cp digitalis.service /etc/systemd/system/
sudo systemctl enable --now digitalis
```

---

## 🔌 HTTP API

| Method | Path | Description |
|--------|------|-------------|
| `GET`   | `/api/torrents` | List all torrents + live status |
| `GET`   | `/api/torrents/{id}` | Detail view: piece map, file progress, health, trackers |
| `GET`   | `/ws` | WebSocket: 1 Hz live ticks (speeds, counters, torrent list) |
| `POST`  | `/api/torrents` | Add: `{"source": "magnet:/http:/base64(.torrent)", "dir": "unix/linux"}` |
| `POST`  | `/api/torrents/{id}/pause` | Pause a torrent |
| `POST`  | `/api/torrents/{id}/resume` | Resume a torrent |
| `POST`  | `/api/torrents/{id}/delete` | Remove a torrent |
| `POST`  | `/api/torrents/{id}/move` | Move to a category: `{"category": "unix/linux/debian"}` |
| `POST`  | `/api/torrents/{id}/trackers` | Add a custom tracker: `{"url": "udp://tracker.opentrackr.org:1337/announce"}` |
| `DELETE`| `/api/torrents/{id}/trackers` | Remove a tracker: `{"url": "..."}` |
| `PATCH` | `/api/torrents/{id}` | Update: `{"download_limit": 500000}` (per-torrent rate cap) |
| `POST`  | `/api/bulk` | Multi-select actions: `{"action": "pause"\|"resume"\|"delete", "ids": ["...","..."]}` |
| `GET`   | `/api/files?path=unix/linux` | Browse the save directory for the in-app file browser |
| `GET`   | `/api/categories` | List category folders + torrent counts |
| `POST`  | `/api/categories` | Create nested folders: `{"path": "unix/linux/debian"}` |
| `DELETE`| `/api/categories` | Delete an empty category: `{"path": "unix"}` |
| `GET`   | `/api/stats` | Dashboard: hourly/daily/monthly/yearly traffic + country connection log |
| `GET`/`POST` | `/api/settings` | Read/update rates, daily upload cap, base dir |
| `GET`   | `/api/trackers` | Global tracker table (announces, success/fail, working) |

```bash
curl -X POST localhost:1919/api/torrents \
  -H 'Content-Type: application/json' \
  -d '{"source": "magnet:?xt=urn:btih:08ada5a...", "dir": "linux/debian"}'
```

---

## 🏗️ Architecture

```
digitalis/
├── bencode/    → hand-rolled bencode encoder/decoder (+ DecodePrefix for BEP-9 splices)
├── metainfo/   → .torrent parser, magnet URI, info-hash (+ raw-info extraction for ut_metadata)
├── tracker/    → HTTP/UDP tracker announce client
├── peerwire/   → peer-wire protocol messages, handshake, BEP-10 extension enabler (reserved bit)
├── storage/    → file layout + per-piece SHA-1 verification
├── geo/        → embedded IP→country table (357k ranges, generated by geo/geobuild)
├── dht/        → self-contained UDP KRPC DHT client (get_peers/announce against 5 routers)
├── torrente/   → core engine: piece picker, sessions, rate limits, detail view, settings,
│                 statistics, magnets (extend.go: ut_metadata + PEX), discovery (discovery.go),
│                 DHT bridge (dhtbridge.go)
│   ├── extend.go       → BEP-9 ut_metadata exchange + BEP-11 PEX messages
│   ├── discovery.go    → peer dialing, magnet→storage upgrade, notifications, per-torrent limits
│   ├── dhtbridge.go    → lazy DHT startup, get_peers crawl, announce loop
│   └── settings.go / stats.go / trackers.go → persistence & aggregation
├── web/        → embedded HTTP server (embed.FS) + multilingual UI
│   ├── web.go  → REST API, torrent detail endpoint, bulk/limit/file-browser handlers
│   ├── persist.go → torrent records (torrents.json) with automatic restore
│   ├── ws.go   → WebSocket broadcast hub (live 1 Hz ticks + completion/metadata notices)
│   └── templates/index.html   → forest-themed Tailwind dashboard (12 languages, guide, toasts, bulk)
└── cmd/digitalis/main.go      → entrypoint, flags, watch folder
```

**Data flow**

```
.torrent/magnet ─▶ metainfo ─▶ storage (pre-allocate files, verify SHA-1)
                    │
                    ▼
                tracker ──announce──▶ peers ──▶ peerwire handshake
                    ▲                          │
                    │              bitfield / request / piece
              piece picker ◀──── engine ◀──────┘
                    └── write blocks ─▶ storage ─▶ verify ─▶ seed
```

---

## 🌍 Localization

The web UI ships with **12 languages** out of the box. A single dropdown switches
the whole interface instantly (RTL support included for Arabic). The choice is
remembered in `localStorage`.

| Language | Code | | Language | Code |
|:--------:|:----:|---|:--------:|:----:|
| English  | `en` | | Português | `pt` |
| Türkçe   | `tr` | | Русский  | `ru` |
| Deutsch  | `de` | | 日本語    | `ja` |
| Français | `fr` | | 中文      | `zh` |
| Español  | `es` | | العربية   | `ar` |
| Italiano | `it` | | हिन्दी    | `hi` |

---

## 🤍 Design Philosophy

- **From scratch** — the protocol is ours, not a wrapper around a C library.
- **Zero bloat** — no Docker, no database, no framework of the week.
- **One binary** — static, embedded UI, ready for a Raspberry Pi or a rack server.
- **Pretty as well as functional** — software should look like the flower it's named after. 🌸

---

## 🗂️ Categories & folders

Every category **is a folder** under the base save directory. Create nested paths
like `unix/linux/debian` and torrents added to that category land in exactly that
folder — the directory tree mirrors your taxonomy, and files are re-arranged
automatically when you move a torrent between categories.

```bash
curl -X POST localhost:1919/api/categories \
  -H 'Content-Type: application/json' \
  -d '{"path": "unix/linux/debian"}'
```

---

## 🔗 Custom trackers

Public trackers can die; keep torrents alive by managing trackers per torrent from
the UI or the API. Any `http://`, `https://`, `udp://` or `ws://` announce URL works,
and the tracker table tracks announces, successes, failures and live seeders.

```bash
curl -X POST localhost:1919/api/torrents/{id}/trackers \
  -H 'Content-Type: application/json' \
  -d '{"url": "udp://tracker.opentrackr.org:1337/announce"}'
```

---

## 🌍 Country log

Every new peer connection is geolocated against a compact, **embedded** IP→country
table (built from the DB-IP database, ~200 KB compressed ranges) — no network calls,
no big query API. The dashboard's *Stats* view shows today's top countries with flag
badges, and totals persist to disk.

---

## ⚙️ Settings & daily upload cap

Rates and the base save directory are stored in the OS config dir
(`~/.config/digitalis/settings.json`). Set a **daily upload cap** (bytes) and once
the client has shared that much in a calendar day it pauses all seeding until
midnight, when the counter resets automatically. Per-torrent counters, announce
statistics and country logs live in `stats.json`, and torrent records (magnet
sources + categories) are kept in `torrents.json` so every torrent comes back after
a restart — no manual re-add.

Give any single torrent its own **download cap** from the detail panel
(`PATCH /api/torrents/{id}`, field `download_limit`); the global setting stays the
ceiling, per-torrent limits run underneath it.

---

## 🧪 Testing

```bash
go vet ./...
go build -o digitalis ./cmd/digitalis
```

Validated against **`webtorrent.io` Sintel** (129 MB, 987 pieces): full download,
SHA-1 verification, and seeding confirmed with live peers. **Magnet add** resolves
metadata over BEP-9 `ut_metadata` directly from peers (trackers + DHT together find
enough peers in seconds), survives restarts, and seeds the same session. Detail
endpoint returns the piece map + per-file progress; the WebSocket handshake, 1 Hz
live ticks and completion/metadata notices verified with a raw `Sec-WebSocket-Key`
upgrade.

---

## 📄 License

**MIT** — do whatever you like, but let it bloom. 🌿

<div align="center">

*Made with ❤️ and a whole lot of foxglove.*

**coded by [Alperen Yavuz](https://github.com/alplix)**

</div>
