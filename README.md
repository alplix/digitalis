# Digitalis (Yüksük Otu)

Sıfırdan yazılmış, minimal bağımlılıklı BitTorrent istemcisi: Go motor + Tailwind web arayüzü.

- İstemci + tracker iletişimi + depolama, harici torrent kütüphanesi olmadan tamamen kendi kodumuz
- Tracker announce (UDP + HTTP), peer wire protokolü, piece seçimi (rare-first), hız sınırlama
- Gömülü web UI (tek binary, Tailwind CDN): torrent ekleme (magnet / .torrent / dosya), durdurma, silme
- Watch dizininden otomatik .torrent alımı
- Doygun indirme + veritabanı/sunucu gerektirmeyen pür precompiled binary

## Kurulum

Bağımlılıklar: Go ≥ 1.23 (yalnızca derlemek için).

```sh
make build   # yoksa: go build -o digitalis ./cmd/digitalis
```

## Çalıştırma

```sh
./digitalis \
  --web :8080 \
  --peer-port 51413 \
  --dir /mnt/torrents/downloads \
  --watch /mnt/torrents/watch \
  --download-limit 0 \
  --upload-limit 0
```

Web UI → `http://SERVER:8080/`

### systemd

```ini
[Unit]
Description=Digitalis BitTorrent Client
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=alp
WorkingDirectory=/home/alp/digitalis
ExecStart=/home/alp/digitalis/digitalis --web :8080 --peer-port 51413 --dir /mnt/torrents/downloads --watch /mnt/torrents/watch
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

Port 51413'ü yönlendiriciden NAT'a açın (gelen peer bağlantıları için).

## API

| Yöntem | Yol | İşlev |
|--------|-----|-------|
| GET | `/api/torrents` | Torrent listesi + durum |
| POST | `/api/torrents` | Ekle: `{"source": "magnet:?... / http(s):... / base64(.torrent)"}` |
| POST | `/api/torrents/{id}/pause` | Duraklat |
| POST | `/api/torrents/{id}/resume` | Devam |
| POST | `/api/torrents/{id}/delete` | Sil |

## Yapı

```
bencode/    Bencode kodlayıcı/çözücü
metainfo/   .torrent/metainfo ayrıştırıcı, magnet URI
tracker/    Tracker announce (UDP/HTTP) istemcisi
peerwire/   Peer wire protokol mesajları
storage/    Dosya düzeni + parça doğrulama (SHA-1)
torrente/   Motor: torrent yönetimi, parça seçim, peer oturumları, hız sınırlama
web/        Gömülü web sunucusu ve arayüz
cmd/        main.go
```

## Test

```sh
go vet ./...
go build ./...
```

Gerçek dünya testi — `webtorrent.io` Sintel ile doğrulandı (129 MB, 987 parça, seeding).

## Lisans

MIT