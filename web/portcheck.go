package web

import (
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

// portCheck answers "is my peer port reachable from the internet?". It learns
// the public IP from ipify, then dials back to itself on the peer port. A
// success means the port accepts connections; a failure can also mean a
// hairpin-NAT limitation of the router, which the UI communicates.
func (s *Server) portCheck(w http.ResponseWriter, r *http.Request) {
	port := s.engine.Port()

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("https://api.ipify.org")
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "public IP lookup failed: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	ipBytes, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "public IP lookup failed: " + err.Error()})
		return
	}
	ip := string(ipBytes)

	start := time.Now()
	conn, dialErr := net.DialTimeout("tcp", net.JoinHostPort(ip, strconv.Itoa(port)), 5*time.Second)
	latency := time.Since(start).Milliseconds()
	reachable := dialErr == nil
	if conn != nil {
		conn.Close()
	}

	result := "open"
	if !reachable {
		result = "closed_or_no_hairpin"
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"public_ip":  ip,
		"port":       port,
		"reachable":  reachable,
		"latency_ms": latency,
		"result":     result,
	})
}
