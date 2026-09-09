package torrente

import (
	"sort"
	"strings"

	"github.com/alplix/digitalis/geo"
)

// PeerView is one connected peer as shown in the torrent detail drawer.
type PeerView struct {
	Addr        string  `json:"addr"`
	Client      string  `json:"client"`
	Country     string  `json:"country"`
	CountryName string  `json:"country_name"`
	Down        int64   `json:"down"`    // bytes received from this peer
	Up          int64   `json:"up"`      // bytes sent to this peer
	Progress    float64 `json:"progress"`
	Choked      bool    `json:"choked"`     // the peer is choking us
	Interested  bool    `json:"interested"` // the peer wants our data
	Seeding     bool    `json:"seeding"`    // we already uploaded to this peer
}

// clientName maps an Azureus-style peer id prefix to a friendly client name.
func clientName(pid []byte) string {
	if len(pid) < 2 {
		return "unknown"
	}
	if pid[0] == '-' && len(pid) >= 7 {
		code := strings.ToUpper(string(pid[1:3]))
		if !isAlpha(pid[1]) || !isAlpha(pid[2]) {
			code = strings.ToUpper(string(pid[1:2]))
		}
		return clientNames[code]
	}
	c := strings.ToUpper(string(pid[0]))
	if isAlpha(pid[0]) {
		if n, ok := clientNames[c]; ok {
			return n
		}
	}
	return "unknown"
}

func isAlpha(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

var clientNames = map[string]string{
	"7T":  "7Torrents",
	"AB":  "AnyEvent BitTorrent",
	"AG":  "Ares",
	"AR":  "Arctic",
	"AT":  "Artemis",
	"AV":  "Avicora",
	"AZ":  "Azureus/Vuze",
	"BB":  "BitBuddy",
	"BC":  "BitComet",
	"BE":  "BareTorrent",
	"BG":  "BTGetit",
	"BM":  "BitMate",
	"BP":  "BitTorrent Pro",
	"BR":  "BitRocket",
	"BS":  "BTSlave",
	"BT":  "BitTorrent (mainline)",
	"BW":  "BitWombat",
	"BX":  "BittorrentX",
	"CD":  "Conduct",
	"CT":  "CTorrent",
	"DE":  "Deluge",
	"DP":  "Propagate Data",
	"EB":  "EBit",
	"EC":  "eCoefficient",
	"ES":  "electric sheep",
	"FC":  "FileCroc",
	"FD":  "Free Download Manager",
	"FT":  "FoxTorrent",
	"FX":  "Freebox BitTorrent",
	"G3":  "G3 Torrent",
	"GT":  "GetTorrent",
	"HL":  "Halite",
	"HN":  "Hydranode",
	"KG":  "KGet",
	"KT":  "KTorrent",
	"LH":  "LHAnt",
	"LP":  "Lphant",
	"LT":  "libtorrent",
	"MO":  "MonoTorrent",
	"MP":  "MooPolice",
	"MR":  "Miro",
	"MT":  "MoonlightTorrent",
	"NB":  "Net::BitTorrent",
	"NX":  "Net Transport",
	"OS":  "OneSwarm",
	"OT":  "OmegaTorrent",
	"PD":  "Pando",
	"PT":  "PHPTracker",
	"qB":  "qBittorrent",
	"QD":  "QBittorrent Dark",
	"RT":  "Retriever",
	"RZ":  "RezTorrent",
	"S~":  "Shareaza",
	"SB":  "SwiftBit",
	"SD":  "Xunlei",
	"SM":  "SohuMedia",
	"SP":  "BitSpirit",
	"SS":  "SwarmScope",
	"ST":  "SymTorrent",
	"SZ":  "Shareaza",
	"TL":  "Tixati",
	"TN":  "TorrentDotNET",
	"TR":  "Transmission",
	"TS":  "TorrentStorm",
	"TT":  "TuoTu",
	"UL":  "uLeecher",
	"UM":  "uTorrent Mac",
	"UT":  "uTorrent",
	"VG":  "Vagaa",
	"WG":  "WGet",
	"WW":  "WebTorrent",
	"WX":  "WX Torrent",
	"XL":  "Xunlei",
	"XT":  "XanTorrent",
	"XX":  "Xtorrent",
	"ZT":  "ZipTorrent",
	"M":   "Mainline BitTorrent",
	"T":   "BitTornado",
	"A":   "ABC",
	"O":   "Osprey",
	"S":   "Shadow's client",
	"R":   "Tribler",
	"U":   "UPnP NAT Bit Torrent",
	"Q":   "BTQueue",
	"B":   "BitBomb",
	"X":   "Xtorrent",
}

// Peers lists the live peer connections of one torrent.
func (e *Engine) Peers(id string) []PeerView {
	e.mu.Lock()
	t, ok := e.torrents[id]
	if !ok {
		e.mu.Unlock()
		return []PeerView{}
	}
	total := 0
	t.mu.Lock()
	total = t.PiecesTotal
	t.mu.Unlock()

	out := []PeerView{}
	for s := range e.sessions {
		if s.t != t {
			continue
		}
		pv := PeerView{
			Addr:       s.addr,
			Down:       s.bytesDown,
			Up:         s.bytesUp,
			Choked:     s.amChoked,
			Interested: s.peerInterested,
			Seeding:    s.seededPeer,
		}
		if s.peerBitfield != nil && total > 0 {
			n := 0
			for i := 0; i < total; i++ {
				if s.peerBitfield.Has(i) {
					n++
				}
			}
			pv.Progress = float64(n) / float64(total)
		}
		pid := s.conn.PeerID()
		pv.Client = clientName(pid[:])
		pv.Country = geo.Lookup(s.addr)
		pv.CountryName = countryName(pv.Country)
		out = append(out, pv)
	}
	e.mu.Unlock()

	sort.Slice(out, func(i, j int) bool { return out[i].Down > out[j].Down })
	return out
}
