package torrente

import (
	"crypto/sha1"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/alplix/digitalis/metainfo"
)

func makeTestMetainfo(name string) *metainfo.MetaInfo {
	piece := sha1.Sum([]byte("dummypiecedata"))
	raw := fmt.Sprintf("d4:name%d:%s12:piece lengthi16384e6:pieces20:%s5:lengthi1e4:test8:whatevere", len(name), name, string(piece[:]))
	info := metainfo.InfoDict{
		Name: name, PieceLength: 16384, Length: 1,
		Pieces: string(piece[:]),
		Raw:    []byte(raw),
	}
	return &metainfo.MetaInfo{Info: info, Announce: "http://127.0.0.1:1/announce"}
}

func makeTestMultiMetainfo(name string) *metainfo.MetaInfo {
	piece := sha1.Sum([]byte("dummypiecedata"))
	// multi-file info: name + one file "sub/a.bin" length 1
	raw := fmt.Sprintf("d4:name%d:%s12:piece lengthi16384e6:pieces20:%s5:filesld6:lengthi1e4:pathl3:sub5:a.bineee4:test8:whatevere",
		len(name), name, string(piece[:]))
	info := metainfo.InfoDict{
		Name: name, PieceLength: 16384,
		Pieces: string(piece[:]),
		Files:  []metainfo.File{{Length: 1, Path: []string{"sub", "a.bin"}}},
		Raw:    []byte(raw),
	}
	return &metainfo.MetaInfo{Info: info, Announce: "http://127.0.0.1:1/announce"}
}

// TestRemoveTorrentWithFiles verifies that deleting a torrent also removes its
// on-disk data when that data lives under a registered storage root, and that
// it does NOT touch data outside the roots.
func TestRemoveTorrentWithFiles(t *testing.T) {
	base, err := os.MkdirTemp("", "trmtest-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)

	outside := filepath.Join(os.TempDir(), "trmtest-outside-"+base[10:])
	os.RemoveAll(outside)
	defer os.RemoveAll(outside)

	e := NewEngine(51000)
	e.Logf = func(string, ...interface{}) {}

	if err := e.AddDisk(base); err != nil {
		t.Fatalf("AddDisk: %v", err)
	}

	// case 1: multi-file torrent data under the registered root gets deleted
	mi := makeTestMultiMetainfo("withfiles")
	tt, err := e.AddTorrent(mi, base)
	if err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	root := filepath.Join(base, "withfiles")
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("storage should have created %s: %v", root, err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "a.bin"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := e.RemoveTorrentWithFiles(tt.ID); err != nil {
		t.Fatalf("RemoveTorrentWithFiles: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("expected data dir removed, still exists: %v", err)
	}

	// case 1b: single-file torrent's data file is deleted too
	mi1b := makeTestMetainfo("singlefile")
	tt1b, err := e.AddTorrent(mi1b, base)
	if err != nil {
		t.Fatalf("AddTorrent1b: %v", err)
	}
	sf := filepath.Join(base, "singlefile")
	if _, err := os.Stat(sf); err != nil {
		t.Fatalf("storage should have created %s: %v", sf, err)
	}
	if err := e.RemoveTorrentWithFiles(tt1b.ID); err != nil {
		t.Fatalf("RemoveTorrentWithFiles1b: %v", err)
	}
	if _, err := os.Stat(sf); !os.IsNotExist(err) {
		t.Fatalf("expected single file removed, still exists: %v", err)
	}

	// case 2: data outside any root is left untouched
	if err := os.MkdirAll(outside, 0755); err != nil {
		t.Fatal(err)
	}
	mi2 := makeTestMetainfo("outside")
	tt2, err := e.AddTorrent(mi2, outside)
	if err != nil {
		t.Fatalf("AddTorrent2: %v", err)
	}
	f := filepath.Join(outside, "outside")
	if _, err := os.Stat(f); err != nil {
		t.Fatalf("storage should have created %s: %v", f, err)
	}
	if err := e.RemoveTorrentWithFiles(tt2.ID); err != nil {
		t.Fatalf("RemoveTorrentWithFiles2: %v", err)
	}
	if _, err := os.Stat(f); os.IsNotExist(err) {
		t.Fatal("outside file was wrongly deleted")
	}
}