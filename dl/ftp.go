package dl

import (
	"bufio"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// Minimal FTP client: enough to stream a RETR transfer in passive mode with
// optional credentials from the URL. Only what the downloader needs.

type ftpConn struct {
	r *bufio.Reader
	w net.Conn
}

// respText reads one reply (multi-line replies consumed fully) and returns
// the numeric code plus the full reply text.
func (c *ftpConn) respText() (int, string, error) {
	line, err := c.r.ReadString('\n')
	if err != nil {
		return 0, "", fmt.Errorf("ftp read: %v", err)
	}
	text := line
	if len(line) >= 4 && line[3] == '-' { // multi-line: "150-..."
		code := line[:3]
		for {
			l2, err2 := c.r.ReadString('\n')
			if err2 != nil {
				return 0, "", err2
			}
			text += l2
			if strings.HasPrefix(l2, code) && len(l2) >= 4 && l2[3] != '-' {
				break
			}
		}
	}
	code, err := strconv.Atoi(strings.TrimSpace(line[:3]))
	if err != nil {
		return 0, "", fmt.Errorf("ftp bad reply: %q", strings.TrimSpace(line))
	}
	return code, text, nil
}

// cmd sends a command and checks the reply code against the accepted set.
// Accepting a single expected code is enough for the commands we use;
// FTP servers answer positive (1xx/2xx) codes that vary per implementation,
// so accept==0 means "any 1xx/2xx".
func (c *ftpConn) cmd(accept int, line string) (int, error) {
	c.w.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := c.w.Write([]byte(line + "\r\n")); err != nil {
		return 0, fmt.Errorf("ftp write: %v", err)
	}
	c.w.SetWriteDeadline(time.Time{})
	code, _, err := c.respText()
	if err != nil {
		return 0, err
	}
	if accept == 0 {
		if code < 100 || code >= 300 {
			return code, fmt.Errorf("ftp %s: got %d", firstWord(line), code)
		}
	} else if code != accept {
		return code, fmt.Errorf("ftp %s: got %d, want %d", firstWord(line), code, accept)
	}
	return code, nil
}

// pasv enters passive mode and returns the data host:port.
func (c *ftpConn) pasv() (string, error) {
	c.w.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := c.w.Write([]byte("PASV\r\n")); err != nil {
		return "", err
	}
	c.w.SetWriteDeadline(time.Time{})
	_, text, err := c.respText()
	if err != nil {
		return "", err
	}
	// "227 Entering Passive Mode (h1,h2,h3,h4,p1,p2)"
	i := strings.IndexByte(text, '(')
	j := strings.IndexByte(text, ')')
	if i < 0 || j < i {
		return "", fmt.Errorf("ftp PASV: cannot parse %q", strings.TrimSpace(text))
	}
	parts := strings.Split(text[i+1 : j], ",")
	if len(parts) != 6 {
		return "", fmt.Errorf("ftp PASV: bad reply %q", text)
	}
	nums := make([]int, 6)
	for k, s := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			return "", fmt.Errorf("ftp PASV: bad number %q", s)
		}
		nums[k] = n
	}
	host := strconv.Itoa(nums[0]) + "." + strconv.Itoa(nums[1]) + "." + strconv.Itoa(nums[2]) + "." + strconv.Itoa(nums[3])
	port := nums[4]*256 + nums[5]
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func firstWord(s string) string {
	if i := strings.IndexByte(s, ' '); i >= 0 {
		return s[:i]
	}
	return s
}

func newBufReader(conn net.Conn) *bufio.Reader {
	return bufio.NewReader(conn)
}
