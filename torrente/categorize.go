package torrente

import (
	"path/filepath"
	"strings"

	"github.com/alplix/digitalis/metainfo"
)

// Smart category detection: Digitalis groups torrents into library folders
// ("movies", "series", "music", "books", "software", "games", "education",
// "other") based on file types and name keywords. Purely heuristic — the user
// can always override with an explicit category folder.

var smartExtCat = map[string]string{
	".mp4": "movies", ".mkv": "movies", ".avi": "movies", ".webm": "movies",
	".mov": "movies", ".m4v": "movies", ".mpg": "movies", ".mpeg": "movies",
	".ts": "movies", ".3gp": "movies", ".ogv": "movies", ".wmv": "movies",
	".flv": "movies",
	".mp3": "music", ".flac": "music", ".wav": "music", ".m4a": "music",
	".aac": "music", ".ogg": "music", ".opus": "music", ".wma": "music",
	".pdf": "books", ".epub": "books", ".mobi": "books", ".djvu": "books",
	".cbr": "books", ".cbz": "books",
	".exe": "software", ".msi": "software", ".iso": "software", ".dmg": "software",
	".deb": "software", ".rpm": "software", ".appimage": "software", ".apk": "software",
	".nsp": "games", ".xci": "games", ".cia": "games", ".nes": "games",
	".snes": "games", ".gba": "games", ".nds": "games",
}

var smartKwRules = []struct {
	kws []string
	cat string
	w   int
}{
	{[]string{"s01e", "s02e", "s03e", "s04e", "s05e", "season ", "episode "}, "series", 8},
	{[]string{"series"}, "series", 8},
	{[]string{"1080p", "720p", "2160p", " x264", " x265", "hevc", "bluray", "web-dl", "webrip", "movie", " film"}, "movies", 4},
	{[]string{"album", "lossless", "discography", "flac ", "320kbps"}, "music", 4},
	{[]string{"portable", "setup", "installer", "crack", "patch ", "keygen", "apk"}, "software", 4},
	{[]string{"repack", "gog", "fitgirl", "dodi", "win64", "ps4", "ps5", " game"}, "games", 4},
	{[]string{"ebook", " book", "magazine", "comic"}, "books", 4},
	{[]string{"udemy", "course", "tutorial", "lecture", "edx", "masterclass"}, "education", 4},
}

// Categorize inspects a torrent's name and file list and returns a suggested
// library folder name. Returns "other" when nothing clearly matches.
func Categorize(name string, files []metainfo.File) string {
	score := make(map[string]int)
	lower := strings.ToLower(strings.ReplaceAll(name, "-", " "))

	extSeen := make(map[string]bool)
	for _, f := range files {
		if len(f.Path) == 0 {
			continue
		}
		e := strings.ToLower(filepath.Ext(f.Path[len(f.Path)-1]))
		if cat, ok := smartExtCat[e]; ok {
			extSeen[cat] = true
		}
	}
	for cat := range extSeen {
		score[cat] += 3
	}

	for _, rule := range smartKwRules {
		matched := false
		for _, kw := range rule.kws {
			if strings.Contains(lower, kw) {
				matched = true
				break
			}
		}
		if matched {
			score[rule.cat] += rule.w
		}
	}

	best := "other"
	bestScore := 0
	for cat, v := range score {
		if v > bestScore {
			best, bestScore = cat, v
		}
	}
	return best
}
