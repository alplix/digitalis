// Command digitalis runs the Digitalis BitTorrent engine and web UI.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/alplix/digitalis/metainfo"
	"github.com/alplix/digitalis/torrente"
	"github.com/alplix/digitalis/web"
)

func main() {
	var (
		webAddr   = flag.String("web", ":8080", "web UI listen address")
		peerPort  = flag.Int("peer-port", 51413, "BitTorrent peer listen port")
		dir       = flag.String("dir", "", "torrent save directory (default: /mnt/torrents/downloads)")
		upLimit   = flag.Int64("upload-limit", 0, "global upload limit in bytes/sec (0=unlimited)")
		downLimit = flag.Int64("download-limit", 0, "global download limit in bytes/sec (0=unlimited)")
		watchDir  = flag.String("watch", "", "watch directory for new .torrent files (optional)")
	)
	flag.Parse()

	if *dir == "" {
		*dir = "/mnt/torrents/downloads"
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "cannot create save dir %s: %v\n", *dir, err)
		os.Exit(1)
	}

	engine := torrente.NewEngine(*peerPort)
	engine.SetUploadLimit(*upLimit)
	engine.SetDownloadLimit(*downLimit)

	if _, err := engine.Listen(); err != nil {
		fmt.Fprintf(os.Stderr, "peer listen failed: %v\n", err)
		os.Exit(1)
	}

	if *watchDir != "" {
		os.MkdirAll(*watchDir, 0o755)
		go watchForTorrents(engine, *watchDir, *dir)
	}

	srv := web.NewServer(engine, *dir)
	httpSrv := &http.Server{Addr: *webAddr, Handler: srv.Handler()}

	go func() {
		engine.Logf("Digitalis web UI on %s", *webAddr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			engine.Logf("web server error: %v", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	engine.Logf("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpSrv.Shutdown(ctx)
	if ln := engine.ListenCloser(); ln != nil {
		ln.Close()
	}
}

// watchForTorrents scans a watch directory for new .torrent files.
func watchForTorrents(e *torrente.Engine, watch, saveDir string) {
	for {
		ents, err := os.ReadDir(watch)
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}
		for _, en := range ents {
			if en.IsDir() || filepath.Ext(en.Name()) != ".torrent" {
				continue
			}
			path := filepath.Join(watch, en.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			mi, err := metainfo.Parse(data)
			if err != nil {
				e.Logf("watch: skip %s: %v", en.Name(), err)
				os.Remove(path)
				continue
			}
			if _, err := e.AddTorrent(mi, saveDir); err != nil {
				e.Logf("watch: %s: %v", en.Name(), err)
				os.Remove(path)
				continue
			}
			os.Remove(path)
			e.Logf("watch: added %s", en.Name())
		}
		time.Sleep(5 * time.Second)
	}
}