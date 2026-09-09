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
	Cat  string `json:"cat"`
}

func Repos() []Repo {
	return []Repo{
		// Linux distributions
		{Name: "Debian", URL: "https://deb.debian.org/debian/", Cat: "linux"},
		{Name: "Debian CD/DVD images", URL: "https://cdimage.debian.org/debian-cd/", Cat: "linux"},
		{Name: "Ubuntu archive", URL: "http://archive.ubuntu.com/ubuntu/", Cat: "linux"},
		{Name: "Ubuntu releases (ISO)", URL: "https://releases.ubuntu.com/", Cat: "linux"},
		{Name: "Ubuntu CD images", URL: "https://cdimage.ubuntu.com/", Cat: "linux"},
		{Name: "Linux Mint", URL: "https://mirrors.edge.kernel.org/linuxmint/", Cat: "linux"},
		{Name: "Fedora", URL: "https://download.fedoraproject.org/pub/fedora/linux/", Cat: "linux"},
		{Name: "Arch Linux", URL: "https://geo.mirror.pkgbuild.com/", Cat: "linux"},
		{Name: "Manjaro", URL: "https://mirror.manjaro.org/repos/", Cat: "linux"},
		{Name: "EndeavourOS", URL: "https://mirror.alpix.eu/endeavouros/", Cat: "linux"},
		{Name: "Alpine Linux", URL: "https://dl-cdn.alpinelinux.org/alpine/", Cat: "linux"},
		{Name: "openSUSE", URL: "https://download.opensuse.org/", Cat: "linux"},
		{Name: "AlmaLinux", URL: "https://repo.almalinux.org/almalinux/", Cat: "linux"},
		{Name: "Rocky Linux", URL: "https://download.rockylinux.org/pub/rocky/", Cat: "linux"},
		{Name: "CentOS Vault", URL: "https://vault.centos.org/", Cat: "linux"},
		{Name: "NixOS", URL: "https://channels.nixos.org/", Cat: "linux"},
		{Name: "Void Linux", URL: "https://repo-default.voidlinux.org/", Cat: "linux"},
		{Name: "Gentoo", URL: "https://distfiles.gentoo.org/", Cat: "linux"},
		{Name: "Slackware", URL: "https://slackware.cs.utah.edu/pub/slackware/", Cat: "linux"},
		{Name: "Kali Linux", URL: "https://http.kali.org/kali/", Cat: "linux"},
		{Name: "MX Linux", URL: "https://mxrepo.com/mx/repo/", Cat: "linux"},
		{Name: "Pop!_OS", URL: "https://iso.pop-os.org/", Cat: "linux"},
		{Name: "elementary OS", URL: "https://builds.elementary.io/", Cat: "linux"},
		{Name: "Deepin", URL: "https://community-packages.deepin.com/deepin/", Cat: "linux"},

		// BSD family
		{Name: "FreeBSD releases", URL: "https://download.freebsd.org/ftp/releases/", Cat: "bsd"},
		{Name: "FreeBSD FTP", URL: "https://download.freebsd.org/ftp/pub/FreeBSD/", Cat: "bsd"},
		{Name: "OpenBSD", URL: "https://cdn.openbsd.org/pub/OpenBSD/", Cat: "bsd"},
		{Name: "NetBSD", URL: "https://cdn.netbsd.org/pub/NetBSD/", Cat: "bsd"},
		{Name: "DragonFly BSD", URL: "https://mirror-master.dragonflybsd.org/dragonfly/", Cat: "bsd"},

		// Software & ISO archives
		{Name: "Kernel.org", URL: "https://cdn.kernel.org/pub/", Cat: "software"},
		{Name: "GNU (ftp.gnu.org)", URL: "https://ftp.gnu.org/gnu/", Cat: "software"},
		{Name: "Apache", URL: "https://downloads.apache.org/", Cat: "software"},
		{Name: "GNOME", URL: "https://download.gnome.org/sources/", Cat: "software"},
		{Name: "KDE", URL: "https://download.kde.org/stable/", Cat: "software"},
		{Name: "Mozilla (archive)", URL: "https://archive.mozilla.org/pub/", Cat: "software"},
		{Name: "LibreOffice", URL: "https://documentfoundation.mirror.garr.it/libreoffice/stable/", Cat: "software"},
		{Name: "VideoLAN (VLC)", URL: "https://get.videolan.org/vlc/", Cat: "software"},
		{Name: "CTAN (TeX)", URL: "https://ftp.fau.de/ctan/", Cat: "software"},
		{Name: "CRAN (R)", URL: "https://cran.r-project.org/", Cat: "software"},
		{Name: "CPAN (Perl)", URL: "https://www.cpan.org/", Cat: "software"},
		{Name: "Eclipse", URL: "https://download.eclipse.org/eclipse/", Cat: "software"},
		{Name: "Rust static builds", URL: "https://static.rust-lang.org/dist/", Cat: "software"},
		{Name: "Go toolchains", URL: "https://go.dev/dl/", Cat: "software"},

		// Misc / testing-friendly mirrors
		{Name: "Kernel test ISOs (netboot)", URL: "https://archive.ubuntu.com/ubuntu/dists/", Cat: "misc"},
		{Name: "Debian netboot", URL: "https://deb.debian.org/debian/dists/stable/main/installer-amd64/", Cat: "misc"},
		{Name: "TUNA mirror (CN)", URL: "https://mirrors.tuna.tsinghua.edu.cn/", Cat: "misc"},
		{Name: "GARR mirror (IT)", URL: "https://mirrors.garr.it/mirrors/", Cat: "misc"},
		{Name: "UAPT mirror (TR)", URL: "https://mirror.veriteknik.com.tr/", Cat: "misc"},
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
