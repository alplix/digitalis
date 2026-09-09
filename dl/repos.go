package dl

import (
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Curated list of public open-source repositories whose mirrors expose
// Apache/nginx style directory indexes. The Downloader page can browse them
// like a file manager and turn any file into a task.

type Repo struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

func Repos() []Repo {
	return []Repo{
		{Name: "Debian", URL: "https://deb.debian.org/debian/"},
		{Name: "Ubuntu", URL: "http://archive.ubuntu.com/ubuntu/"},
		{Name: "FreeBSD", URL: "https://download.freebsd.org/ftp/releases/"},
		{Name: "Fedora", URL: "https://download.fedoraproject.org/pub/fedora/linux/"},
		{Name: "Arch Linux", URL: "https://geo.mirror.pkgbuild.com/"},
		{Name: "Alpine Linux", URL: "https://dl-cdn.alpinelinux.org/alpine/"},
		{Name: "openSUSE", URL: "https://download.opensuse.org/"},
		{Name: "AlmaLinux", URL: "https://repo.almalinux.org/almalinux/"},
		{Name: "Rocky Linux", URL: "https://download.rockylinux.org/pub/rocky/"},
		{Name: "Linux Kernel", URL: "https://cdn.kernel.org/pub/"},
		{Name: "NixOS", URL: "https://channels.nixos.org/"},
		{Name: "Raspberry Pi OS", URL: "https://downloads.raspberrypi.org/"},
		{Name: "CentOS Vault", URL: "https://vault.centos.org/"},
		{Name: "Manjaro", URL: "https://mirror.manjaro.org/repos/"},
	}
}

type RepoEntry struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`    // parsed from index text when present
	SizeH string `json:"size_h"`  // human text as shown by the mirror
}

var anchorRe = regexp.MustCompile(`(?is)<a\s+[^>]*href="([^"]+)"[^>]*>(.*?)</a>([^<]*)`)

// BrowseRepo fetches a directory index page and extracts its links.
func BrowseRepo(dirURL, proxy string) ([]RepoEntry, error) {
	u, err := url.Parse(dirURL)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
		dirURL = u.String()
	}
	client := newClient(proxy)
	resp, err := client.Get(dirURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	return parseIndex(dirURL, string(body)), nil
}

// parseIndex extracts anchors from Apache/nginx autoindex HTML.
func parseIndex(baseURL, body string) []RepoEntry {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	out := []RepoEntry{}
	for _, m := range anchorRe.FindAllStringSubmatch(body, -1) {
		href := strings.TrimSpace(m[1])
		trail := strings.TrimSpace(strings.ReplaceAll(m[3], "&nbsp;", " "))
		if href == "" || strings.HasPrefix(href, "?") || strings.HasPrefix(href, "#") {
			continue
		}
		ref, err := url.Parse(href)
		if err != nil {
			continue
		}
		resolved := base.ResolveReference(ref)
		if resolved.Path < base.Path { // parent links and cross-tree jumps
			continue
		}
		name := strings.TrimSuffix(resolved.Path, "/")
		if i := strings.LastIndexByte(name, '/'); i >= 0 {
			name = name[i+1:]
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		isDir := strings.HasSuffix(href, "/") || strings.HasSuffix(resolved.Path, "/")
		e := RepoEntry{Name: name, URL: resolved.String(), IsDir: isDir}
		if !isDir {
			e.Size, e.SizeH = parseSizeHint(trail)
		}
		out = append(out, e)
	}
	sortEntries(out)
	return out
}

// parseSizeHint picks a human size token like "1.2M" / "450K" / "3.4G".
func parseSizeHint(trail string) (int64, string) {
	fields := strings.Fields(trail)
	for _, f := range fields {
		f = strings.TrimSuffix(f, "</td>")
		if len(f) < 2 {
			continue
		}
		unit := strings.ToUpper(f[len(f)-1:])
		var mult int64
		switch unit {
		case "K":
			mult = 1024
		case "M":
			mult = 1024 * 1024
		case "G":
			mult = 1024 * 1024 * 1024
		case "T":
			mult = 1024 * 1024 * 1024 * 1024
		default:
			continue
		}
		num, err := strconv.ParseFloat(f[:len(f)-1], 64)
		if err != nil || num < 0 {
			continue
		}
		return int64(num * float64(mult)), f
	}
	return 0, ""
}

// sortEntries orders dirs first then names.
func sortEntries(list []RepoEntry) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && repoLess(list[j], list[j-1]); j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

func repoLess(a, b RepoEntry) bool {
	if a.IsDir != b.IsDir {
		return a.IsDir
	}
	return strings.ToLower(a.Name) < strings.ToLower(b.Name)
}
