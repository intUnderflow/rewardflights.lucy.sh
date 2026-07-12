package alertapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/alertstore"
)

const (
	endpoint = "https://fcm.googleapis.com/fcm/send/abc123"
	origin   = "https://rewardflights.lucy.sh"
)

// testAPI builds a handler over a fresh store, with a controllable clock.
func testAPI(t *testing.T) (http.Handler, *alertstore.Store, *time.Time) {
	t.Helper()
	store, err := alertstore.Open(alertstore.Options{
		Path: filepath.Join(t.TempDir(), "subs.json"), Debounce: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := time.Unix(1783810462, 0)
	srv := New(Config{
		Store: store, RatePerMin: 60, Burst: 20,
		Now: func() time.Time { return now },
	})
	return srv.Handler(), store, &now
}

// do issues a request and returns the recorder plus decoded JSON body.
func do(t *testing.T, h http.Handler, method, target, body string, headers map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var decoded map[string]any
	json.Unmarshal(rec.Body.Bytes(), &decoded)
	return rec, decoded
}

func subscribeBody(topics ...string) string {
	quoted := make([]string, len(topics))
	for i, topic := range topics {
		quoted[i] = strconv.Quote(topic)
	}
	return fmt.Sprintf(`{"endpoint":%q,"p256dh":"cGsx","auth":"YXV0aA","topics":[%s]}`,
		endpoint, strings.Join(quoted, ","))
}

func TestSubscribeUnsubscribeTopicsHealth(t *testing.T) {
	h, store, _ := testAPI(t)

	// Subscribe.
	rec, body := do(t, h, "POST", "/subscribe", subscribeBody("rf_LON-TYO_rt_C", "rf_LON-TYO_ow_C"), nil)
	if rec.Code != http.StatusOK || body["ok"] != true {
		t.Fatalf("subscribe: %d %v", rec.Code, body)
	}
	topics, _ := body["topics"].([]any)
	if len(topics) != 2 || topics[0] != "rf_LON-TYO_ow_C" {
		t.Errorf("returned topics = %v (want sorted)", body["topics"])
	}
	if store.Count() != 1 {
		t.Errorf("store count = %d", store.Count())
	}
	if len(store.Get("rf_LON-TYO_rt_C")) != 1 {
		t.Error("sender lookup must see the new subscription")
	}

	// GET /topics.
	rec, body = do(t, h, "GET", "/topics?endpoint="+endpoint, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("topics: %d", rec.Code)
	}
	if got, _ := body["topics"].([]any); len(got) != 2 {
		t.Errorf("topics = %v", body["topics"])
	}
	// Unknown endpoint -> empty list, not null (clients iterate it).
	_, body = do(t, h, "GET", "/topics?endpoint=https://fcm.googleapis.com/fcm/send/nobody", "", nil)
	if got, ok := body["topics"].([]any); !ok || len(got) != 0 {
		t.Errorf("unknown endpoint topics = %#v, want []", body["topics"])
	}
	rec, _ = do(t, h, "GET", "/topics", "", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing endpoint: %d, want 400", rec.Code)
	}

	// Re-subscribe REPLACES the topic set.
	_, body = do(t, h, "POST", "/subscribe", subscribeBody("rf_NYC-LON_ow_M"), nil)
	if got, _ := body["topics"].([]any); len(got) != 1 || got[0] != "rf_NYC-LON_ow_M" {
		t.Errorf("replace returned %v", body["topics"])
	}
	if len(store.Get("rf_LON-TYO_rt_C")) != 0 {
		t.Error("replaced topic must leave the index")
	}

	// Healthz.
	rec, body = do(t, h, "GET", "/healthz", "", nil)
	if rec.Code != http.StatusOK || body["ok"] != true || body["subs"] != float64(1) || body["topics"] != float64(1) {
		t.Errorf("healthz: %d %v", rec.Code, body)
	}

	// Unsubscribe (idempotent).
	rec, body = do(t, h, "POST", "/unsubscribe", fmt.Sprintf(`{"endpoint":%q}`, endpoint), nil)
	if rec.Code != http.StatusOK || body["ok"] != true {
		t.Fatalf("unsubscribe: %d %v", rec.Code, body)
	}
	if store.Count() != 0 {
		t.Errorf("store count after unsubscribe = %d", store.Count())
	}
	rec, _ = do(t, h, "POST", "/unsubscribe", fmt.Sprintf(`{"endpoint":%q}`, endpoint), nil)
	if rec.Code != http.StatusOK {
		t.Errorf("repeat unsubscribe must be idempotent: %d", rec.Code)
	}
	rec, _ = do(t, h, "POST", "/unsubscribe", `{"endpoint":""}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty endpoint: %d, want 400", rec.Code)
	}
}

func TestSubscribeRejects(t *testing.T) {
	h, store, _ := testAPI(t)
	cases := []struct {
		name, body string
		want       int
	}{
		{"bad host", `{"endpoint":"https://evil.example.com/x","p256dh":"cGsx","auth":"YXV0aA","topics":["rf_LON-TYO_ow_C"]}`, http.StatusBadRequest},
		{"http endpoint", `{"endpoint":"http://fcm.googleapis.com/x","p256dh":"cGsx","auth":"YXV0aA","topics":["rf_LON-TYO_ow_C"]}`, http.StatusBadRequest},
		{"bad topic", subscribeBody("rf_LON-TYO_ow_Z"), http.StatusBadRequest},
		{"missing keys", fmt.Sprintf(`{"endpoint":%q,"topics":["rf_LON-TYO_ow_C"]}`, endpoint), http.StatusBadRequest},
		{"invalid json", `{nope`, http.StatusBadRequest},
		{"unknown field", fmt.Sprintf(`{"endpoint":%q,"p256dh":"cGsx","auth":"YXV0aA","topics":[],"admin":true}`, endpoint), http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, body := do(t, h, "POST", "/subscribe", tc.body, nil)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (%v)", rec.Code, tc.want, body)
			}
			if body["ok"] != false {
				t.Errorf("body must report ok:false, got %v", body)
			}
		})
	}
	if store.Count() != 0 {
		t.Errorf("rejected requests must store nothing (count = %d)", store.Count())
	}
}

func TestTooManyTopicsAndFullStore(t *testing.T) {
	// Over-cap topic list -> 400.
	h, _, _ := testAPI(t)
	many := make([]string, alertstore.MaxTopicsPerSub+1)
	for i := range many {
		many[i] = fmt.Sprintf("rf_%s-TYO_ow_C", string([]byte{byte('A' + i/26), byte('A' + i%26), 'X'}))
	}
	rec, _ := do(t, h, "POST", "/subscribe", subscribeBody(many...), nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("over-cap topics: %d, want 400", rec.Code)
	}

	// Full store -> 429.
	store, err := alertstore.Open(alertstore.Options{
		Path: filepath.Join(t.TempDir(), "subs.json"), MaxSubs: 1, Debounce: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	full := New(Config{Store: store}).Handler()
	if rec, _ := do(t, full, "POST", "/subscribe", subscribeBody("rf_LON-TYO_ow_C"), nil); rec.Code != http.StatusOK {
		t.Fatalf("first subscribe: %d", rec.Code)
	}
	body := `{"endpoint":"https://fcm.googleapis.com/fcm/send/second","p256dh":"cGsx","auth":"YXV0aA","topics":["rf_LON-TYO_ow_C"]}`
	rec, decoded := do(t, full, "POST", "/subscribe", body, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("full store: %d, want 429 (%v)", rec.Code, decoded)
	}
}

func TestBodyCap(t *testing.T) {
	h, _, _ := testAPI(t)
	huge := fmt.Sprintf(`{"endpoint":%q,"p256dh":%q,"auth":"YXV0aA","topics":[]}`,
		endpoint, strings.Repeat("A", 20<<10))
	rec, _ := do(t, h, "POST", "/subscribe", huge, nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: %d, want 413", rec.Code)
	}
}

func TestCORS(t *testing.T) {
	h, _, _ := testAPI(t)

	// Preflight from the production origin.
	rec, _ := do(t, h, "OPTIONS", "/subscribe", "", map[string]string{
		"Origin":                        origin,
		"Access-Control-Request-Method": "POST",
	})
	if rec.Code != http.StatusNoContent {
		t.Errorf("preflight status = %d", rec.Code)
	}
	head := rec.Header()
	if head.Get("Access-Control-Allow-Origin") != origin {
		t.Errorf("ACAO = %q", head.Get("Access-Control-Allow-Origin"))
	}
	if !strings.Contains(head.Get("Access-Control-Allow-Methods"), "POST") ||
		!strings.Contains(head.Get("Access-Control-Allow-Methods"), "OPTIONS") {
		t.Errorf("ACAM = %q", head.Get("Access-Control-Allow-Methods"))
	}
	if !strings.Contains(strings.ToLower(head.Get("Access-Control-Allow-Headers")), "content-type") {
		t.Errorf("ACAH = %q", head.Get("Access-Control-Allow-Headers"))
	}
	if head.Get("Vary") != "Origin" {
		t.Errorf("Vary = %q", head.Get("Vary"))
	}

	// Real request from the production origin carries ACAO too.
	rec, _ = do(t, h, "POST", "/subscribe", subscribeBody("rf_LON-TYO_ow_C"), map[string]string{"Origin": origin})
	if rec.Code != http.StatusOK || rec.Header().Get("Access-Control-Allow-Origin") != origin {
		t.Errorf("post: %d ACAO=%q", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}

	// Dev origins allowed.
	for _, dev := range []string{"http://127.0.0.1:8080", "http://localhost:3000"} {
		rec, _ = do(t, h, "OPTIONS", "/subscribe", "", map[string]string{"Origin": dev})
		if rec.Header().Get("Access-Control-Allow-Origin") != dev {
			t.Errorf("dev origin %s: ACAO = %q", dev, rec.Header().Get("Access-Control-Allow-Origin"))
		}
	}

	// A hostile origin gets NO CORS headers (browser blocks the response).
	for _, bad := range []string{"https://evil.example", "https://rewardflights.lucy.sh.evil.example", "null"} {
		rec, _ = do(t, h, "OPTIONS", "/subscribe", "", map[string]string{"Origin": bad})
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("origin %s must not be allowed, got ACAO %q", bad, got)
		}
	}
}

func TestRateLimit(t *testing.T) {
	h, _, now := testAPI(t)
	headers := map[string]string{"CF-Connecting-IP": "203.0.113.7"}

	// Burst of 20 is allowed, the 21st is throttled.
	for i := range 20 {
		if rec, _ := do(t, h, "GET", "/healthz", "", headers); rec.Code != http.StatusOK {
			t.Fatalf("request %d: %d, want 200", i+1, rec.Code)
		}
	}
	rec, body := do(t, h, "GET", "/healthz", "", headers)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("21st request: %d, want 429", rec.Code)
	}
	if body["ok"] != false {
		t.Errorf("429 body = %v", body)
	}
	retry, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || retry < 1 {
		t.Errorf("Retry-After = %q", rec.Header().Get("Retry-After"))
	}

	// A different client IP has its own bucket.
	if rec, _ := do(t, h, "GET", "/healthz", "", map[string]string{"CF-Connecting-IP": "198.51.100.9"}); rec.Code != http.StatusOK {
		t.Errorf("second client: %d, want 200 (per-IP buckets)", rec.Code)
	}

	// Refill: at 60/min, one second buys one token.
	*now = now.Add(time.Second)
	if rec, _ := do(t, h, "GET", "/healthz", "", headers); rec.Code != http.StatusOK {
		t.Errorf("after refill: %d, want 200", rec.Code)
	}
	if rec, _ := do(t, h, "GET", "/healthz", "", headers); rec.Code != http.StatusTooManyRequests {
		t.Errorf("refill grants exactly one token: %d, want 429", rec.Code)
	}
}

func TestMethodRouting(t *testing.T) {
	h, _, _ := testAPI(t)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/subscribe"},
		{"POST", "/topics"},
		{"DELETE", "/subscribe"},
		{"GET", "/nope"},
	} {
		rec, _ := do(t, h, tc.method, tc.path, "", nil)
		if rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404/405", tc.method, tc.path, rec.Code)
		}
	}
}
