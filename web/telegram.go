package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Telegram notifications: a tiny client for the Bot API. Only three events
// trigger messages today: download complete, share goal reached and the disk
// guard engaging. Failures are logged and never affect torrent flow.

type telegramClient struct {
	http *http.Client
}

var tgHTTP = &telegramClient{http: &http.Client{Timeout: 10 * time.Second}}

// sendTelegram delivers text to the configured chat when notifications are
// enabled; otherwise it is a no-op.
func (s *Server) sendTelegram(text string) {
	v := s.engine.View()
	if !v.TelegramEnabled || v.TelegramToken == "" || v.TelegramChat == "" {
		return
	}
	if err := tgHTTP.send(v.TelegramToken, v.TelegramChat, text); err != nil {
		s.engine.Logf("telegram: %v", err)
	}
}

func (c *telegramClient) send(token, chat, text string) error {
	body, err := json.Marshal(map[string]string{
		"chat_id": chat,
		"text":    text,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST",
		fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token),
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram API status %d", resp.StatusCode)
	}
	return nil
}

// testTelegram sends a probe message so the user can verify the bot setup.
func (s *Server) testTelegram(w http.ResponseWriter, r *http.Request) {
	v := s.engine.View()
	if v.TelegramToken == "" || v.TelegramChat == "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "bot token / chat not configured"})
		return
	}
	if err := tgHTTP.send(v.TelegramToken, v.TelegramChat, "digitalis: test message ✓"); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "sent"})
}
