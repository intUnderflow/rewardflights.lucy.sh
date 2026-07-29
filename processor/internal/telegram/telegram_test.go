package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func mockAPI(t *testing.T, handler func(method string, body map[string]any) (int, string)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		method := parts[len(parts)-1]
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		status, resp := handler(method, body)
		w.WriteHeader(status)
		w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	c := New("test-token")
	c.Base = srv.URL
	return c
}

func TestSendFormatsAndSucceeds(t *testing.T) {
	var got map[string]any
	c := mockAPI(t, func(method string, body map[string]any) (int, string) {
		if method != "sendMessage" {
			t.Errorf("method = %s", method)
		}
		got = body
		return 200, `{"ok":true,"result":{}}`
	})
	gone, err := c.Send(context.Background(), 42, `First <seats> open: LON ⇄ TYO`, "1 new: 3–14 Oct", "https://rewardflights.lucy.sh/trip/LON-TYO", "Open the pair")
	if err != nil || gone {
		t.Fatalf("send: gone=%v err=%v", gone, err)
	}
	text := got["text"].(string)
	if !strings.Contains(text, "<b>First &lt;seats&gt; open: LON ⇄ TYO</b>") {
		t.Errorf("title not escaped+bold: %q", text)
	}
	if got["chat_id"].(float64) != 42 || got["parse_mode"] != "HTML" {
		t.Errorf("envelope: %v", got)
	}
	kb := got["reply_markup"].(map[string]any)["inline_keyboard"].([]any)
	btn := kb[0].([]any)[0].(map[string]any)
	if btn["url"] != "https://rewardflights.lucy.sh/trip/LON-TYO" {
		t.Errorf("button: %v", btn)
	}
}

func TestSendClassifiesGone(t *testing.T) {
	c := mockAPI(t, func(method string, body map[string]any) (int, string) {
		return 403, `{"ok":false,"error_code":403,"description":"Forbidden: bot was blocked by the user"}`
	})
	gone, err := c.Send(context.Background(), 42, "t", "b", "", "")
	if !gone || err == nil {
		t.Fatalf("blocked chat: gone=%v err=%v (want gone with error)", gone, err)
	}
	c2 := mockAPI(t, func(method string, body map[string]any) (int, string) {
		return 500, `{"ok":false,"error_code":500,"description":"boom"}`
	})
	gone, err = c2.Send(context.Background(), 42, "t", "b", "", "")
	if gone || err == nil {
		t.Fatalf("transient error: gone=%v err=%v (want retryable)", gone, err)
	}
}

func TestPollDispatchesAndAdvancesOffset(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	c := mockAPI(t, func(method string, body map[string]any) (int, string) {
		if method != "getUpdates" {
			return 200, `{"ok":true,"result":{}}`
		}
		calls++
		if calls == 1 {
			if body["offset"].(float64) != 0 {
				t.Errorf("first offset: %v", body["offset"])
			}
			return 200, `{"ok":true,"result":[
				{"update_id":10,"message":{"text":"/start abc","chat":{"id":7,"type":"private"}}},
				{"update_id":11,"message":{"text":"/list","chat":{"id":7,"type":"private"}}}]}`
		}
		// The second poll proves the offset advanced past the batch; stop
		// the loop from HERE so the assertion can't race the cancel.
		if body["offset"].(float64) != 12 {
			t.Errorf("offset after batch: %v", body["offset"])
		}
		cancel()
		return 200, `{"ok":true,"result":[]}`
	})
	var seen []string
	c.Poll(ctx, func(string, ...any) {}, func(chatID int64, text string) {
		seen = append(seen, text)
	})
	if len(seen) != 2 || seen[0] != "/start abc" || seen[1] != "/list" {
		t.Fatalf("dispatched: %v", seen)
	}
	if calls < 2 {
		t.Fatalf("poll loop stopped early: %d calls", calls)
	}
}
