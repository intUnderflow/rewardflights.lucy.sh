package alertapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/alertstore"
	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/tglink"
)

func tgServer(t *testing.T, withBot bool) (*Server, *tglink.Pending) {
	t.Helper()
	store, err := alertstore.Open(alertstore.Options{
		Path: filepath.Join(t.TempDir(), "subs.json"), Debounce: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	cfg := Config{Store: store, Logf: func(string, ...any) {}}
	var pending *tglink.Pending
	if withBot {
		pending = tglink.New()
		cfg.TelegramLink = pending
		cfg.BotUsername = "RewardFlightsBot"
	}
	return New(cfg), pending
}

func postJSON(t *testing.T, s *Server, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", path, bytes.NewReader(raw))
	req.Header.Set("Origin", "https://rewardflights.lucy.sh")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func TestTelegramLinkMintsCode(t *testing.T) {
	s, pending := tgServer(t, true)
	res := postJSON(t, s, "/telegram/link", map[string]any{
		"watches": []map[string]any{{"route": "LON-TYO", "kind": "rt", "cabins": []string{"C"}}},
	})
	if res.Code != 200 {
		t.Fatalf("status %d: %s", res.Code, res.Body)
	}
	var out struct {
		URL string `json:"url"`
	}
	json.NewDecoder(res.Body).Decode(&out)
	if !strings.HasPrefix(out.URL, "https://t.me/RewardFlightsBot?start=") {
		t.Fatalf("url = %q", out.URL)
	}
	code := strings.TrimPrefix(out.URL, "https://t.me/RewardFlightsBot?start=")
	watches, ok := pending.Take(code)
	if !ok || len(watches) != 1 || watches[0].Route != "LON-TYO" || watches[0].ID == "" {
		t.Fatalf("redeem: %v %v", watches, ok)
	}
}

func TestTelegramLinkValidatesAndGates(t *testing.T) {
	s, _ := tgServer(t, true)
	if res := postJSON(t, s, "/telegram/link", map[string]any{
		"watches": []map[string]any{{"route": "junk", "kind": "rt", "cabins": []string{"C"}}},
	}); res.Code != 400 {
		t.Fatalf("bad watch: %d", res.Code)
	}
	if res := postJSON(t, s, "/telegram/link", map[string]any{"watches": []map[string]any{}}); res.Code != 400 {
		t.Fatalf("empty list: %d", res.Code)
	}
	off, _ := tgServer(t, false)
	if res := postJSON(t, off, "/telegram/link", map[string]any{
		"watches": []map[string]any{{"route": "LON-TYO", "kind": "ow", "cabins": []string{"C"}}},
	}); res.Code != http.StatusServiceUnavailable {
		t.Fatalf("no bot: %d", res.Code)
	}
}

func TestPublicRoutesRejectTelegramEndpoints(t *testing.T) {
	s, _ := tgServer(t, true)
	if res := postJSON(t, s, "/unsubscribe", map[string]any{"endpoint": "telegram:42"}); res.Code != 400 {
		t.Fatalf("unsubscribe: %d %s", res.Code, res.Body)
	}
	if res := postJSON(t, s, "/ack", map[string]any{"endpoint": "telegram:42"}); res.Code != 400 {
		t.Fatalf("ack: %d %s", res.Code, res.Body)
	}
	req := httptest.NewRequest("GET", "/watches?endpoint=telegram:42", nil)
	req.Header.Set("Origin", "https://rewardflights.lucy.sh")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("watches: %d %s", w.Code, w.Body)
	}
}
