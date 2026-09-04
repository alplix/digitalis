<div align="center">

<img src="https://raw.githubusercontent.com/alplix/digitalis/main/docs/logo.svg" alt="Digitalis" width="160"/>

# 🌸 Digitalis

> The **Yüksük Otu** — a blazing-fast BitTorrent client written **from scratch** in Go,
> with a gorgeous multilingual **foxglove-themed** web UI.

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
| 🎯 | **Rare-first picking** | Smart piece selection for faster, fairer downloads |
| 🌐 | **Multilingual UI** | **12 languages** with a live switcher: EN, TR, DE, FR, ES, IT, PT, RU, JA, ZH, AR, HI |
| 🗂️ | **Categories** | Nested folders (`unix/linux/debian`), created on the fly, save dir per torrent, drag-free moving |
| 🔌 | **Flexible input** | Raw `.torrent`, magnet URI, or file upload — paste in bulk |
| 📁 | **Watch folder** | Drop `.torrent` files in a directory — auto-added & seeded |
| 🚀 | **Speed control** | Global download & upload rate limits |
| 💾 | **Reboot-proof** | Ships with a ready-made `systemd` unit |
| 🎨 | **Themer's dream** | Deep plum-magenta "foxglove" dark theme, fully responsive |

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
| `POST`  | `/api/torrents` | Add: `{"source": "magnet:/http:/base64(.torrent)", "dir": "unix/linux"}` |
| `POST`  | `/api/torrents/{id}/pause` | Pause a torrent |
| `POST`  | `/api/torrents/{id}/resume` | Resume a torrent |
| `POST`  | `/api/torrents/{id}/delete` | Remove a torrent |
| `POST`  | `/api/torrents/{id}/move` | Move to a category: `{"category": "unix/linux/debian"}` |
| `GET`   | `/api/categories` | List category folders + torrent counts |
| `POST`  | `/api/categories` | Create nested folders: `{"path": "unix/linux/debian"}` |
| `DELETE`| `/api/categories` | Delete an empty category: `{"path": "unix"}` |

```bash
curl -X POST localhost:1919/api/torrents \
  -H 'Content-Type: application/json' \
  -d '{"source": "magnet:?xt=urn:btih:08ada5a...", "dir": "linux/debian"}'
```

---

## 🏗️ Architecture

```
digitalis/
├── bencode/    → hand-rolled bencode encoder/decoder
├── metainfo/   → .torrent parser, magnet URI, info-hash
├── tracker/    → HTTP/UDP tracker announce client
├── peerwire/   → peer-wire protocol messages & handshake
├── storage/    → file layout + per-piece SHA-1 verification
├── torrente/   → core engine: piece picker, sessions, rate limits
├── web/        → embedded HTTP server (embed.FS) + multilingual UI
│   └── templates/index.html   → Tailwind dashboard (12 languages)
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

## 🧪 Testing

```bash
go vet ./...
go build -o digitalis ./cmd/digitalis
```

Validated against **`webtorrent.io` Sintel** (129 MB, 987 pieces): full download,
SHA-1 verification, and seeding confirmed with live peers.

---

## 📄 License

**MIT** — do whatever you like, but let it bloom. 🌿

<div align="center">

*Made with ❤️ and a whole lot of foxglove.*

</div>
