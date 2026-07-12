package alertapi

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/alertstore"
	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/webpush"
)

const (
	endpoint = "https://fcm.googleapis.com/fcm/send/abc123"
	origin   = "https://rewardflights.lucy.sh"
)

// The API clock: 2026-07-11 (so "today" is well before the October fixtures).
var apiNow = time.Unix(1783810462, 0)

func testAPI(t *testing.T) (http.Handler, *alertstore.Store, *time.Time) {
	t.Helper()
	store, err := alertstore.Open(alertstore.Options{
		Path: filepath.Join(t.TempDir(), "subs.json"), Debounce: time.Millisecond,
		Now: func() time.Time { return apiNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := apiNow
	srv := New(Config{
		Store: store, RatePerMin: 600, Burst: 200,
		Now: func() time.Time { return now },
	})
	return srv.Handler(), store, &now
}

func do(t *testing.T, h http.Handler, method, target, body string, headers map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
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

// subscribeBody builds a v2 body around a watches JSON fragment.
func subscribeBody(watches string) string {
	return fmt.Sprintf(`{"endpoint":%q,"p256dh":"cGsx","auth":"YXV0aA","watches":[%s]}`, endpoint, watches)
}

const (
	unboundedRT  = `{"route":"LON-TYO","kind":"rt","cabins":["C"]}`
	constrainedW = `{"route":"LON-TYO","kind":"rt","cabins":["C"],
	  "out":{"from":"2026-10-01","to":"2026-10-20"},
	  "ret":{"from":"2026-10-10","to":"2026-10-31"},
	  "nights":{"min":7,"max":14}}`
)

func watchesOf(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["watches"].([]any)
	if !ok {
		t.Fatalf("no watches in response: %v", body)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, w := range raw {
		out = append(out, w.(map[string]any))
	}
	return out
}

func TestSubscribeWatches(t *testing.T) {
	h, store, _ := testAPI(t)

	rec, body := do(t, h, "POST", "/subscribe", subscribeBody(unboundedRT+","+constrainedW), nil)
	if rec.Code != http.StatusOK || body["ok"] != true {
		t.Fatalf("subscribe: %d %v", rec.Code, body)
	}
	watches := watchesOf(t, body)
	if len(watches) != 2 {
		t.Fatalf("got %d watches, want 2", len(watches))
	}
	for _, w := range watches {
		if id, _ := w["id"].(string); len(id) != 8 {
			t.Errorf("watch id = %v, want 8 hex chars", w["id"])
		}
		if w["status"] != alertstore.StatusActive {
			t.Errorf("status = %v, want active", w["status"])
		}
		if w["createdAt"] == nil {
			t.Errorf("createdAt must be set by the server: %v", w)
		}
	}
	if store.WatchCount() != 2 {
		t.Errorf("store holds %d watches", store.WatchCount())
	}

	// GET /watches returns them with status.
	rec, body = do(t, h, "GET", "/watches?endpoint="+endpoint, "", nil)
	if rec.Code != http.StatusOK || len(watchesOf(t, body)) != 2 {
		t.Fatalf("GET /watches: %d %v", rec.Code, body)
	}
	// Unknown endpoint -> empty list, not null.
	_, body = do(t, h, "GET", "/watches?endpoint=https://fcm.googleapis.com/fcm/send/nobody", "", nil)
	if got := watchesOf(t, body); len(got) != 0 {
		t.Errorf("unknown endpoint = %v, want []", got)
	}
	rec, _ = do(t, h, "GET", "/watches", "", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing endpoint: %d, want 400", rec.Code)
	}

	// healthz counts watches.
	_, body = do(t, h, "GET", "/healthz", "", nil)
	if body["subs"] != float64(1) || body["watches"] != float64(2) {
		t.Errorf("healthz = %v", body)
	}

	// A v2 write replaces the whole list.
	_, body = do(t, h, "POST", "/subscribe", subscribeBody(unboundedRT), nil)
	if len(watchesOf(t, body)) != 1 {
		t.Errorf("v2 write must replace the list: %v", body)
	}
	// An explicit empty list turns everything off.
	rec, body = do(t, h, "POST", "/subscribe",
		fmt.Sprintf(`{"endpoint":%q,"p256dh":"cGsx","auth":"YXV0aA","watches":[]}`, endpoint), nil)
	if rec.Code != http.StatusOK || store.WatchCount() != 0 {
		t.Errorf("watches:[] must clear the list: %d, %d watches", rec.Code, store.WatchCount())
	}
}

// TestStaleClientCannotDeleteConstrainedWatch is §9-22 at the HTTP layer.
func TestStaleClientCannotDeleteConstrainedWatch(t *testing.T) {
	h, store, _ := testAPI(t)
	if _, body := do(t, h, "POST", "/subscribe", subscribeBody(unboundedRT+","+constrainedW), nil); body["ok"] != true {
		t.Fatal("setup failed")
	}

	// The stale client asks what it has: it sees ONLY the unbounded watch.
	_, body := do(t, h, "GET", "/topics?endpoint="+endpoint, "", nil)
	topics, _ := body["topics"].([]any)
	if len(topics) != 1 || topics[0] != "rf_LON-TYO_rt_C" {
		t.Fatalf("legacy projection = %v, want just the unbounded watch", body["topics"])
	}

	// It edits that list and POSTs the whole thing back, the only way it knows.
	rec, body := do(t, h, "POST", "/subscribe",
		fmt.Sprintf(`{"endpoint":%q,"p256dh":"cGsx","auth":"YXV0aA","topics":["rf_NYC-LON_ow_M"]}`, endpoint), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy write: %d %v", rec.Code, body)
	}

	// The constrained watch SURVIVES; the topic-representable one was replaced.
	watches := store.Watches(endpoint)
	if len(watches) != 2 {
		t.Fatalf("got %d watches, want 2 (the new legacy one + the preserved constrained one)", len(watches))
	}
	var constrained bool
	for _, w := range watches {
		if w.Out != nil && w.Out.To == "2026-10-20" && w.Nights != nil && w.Nights.Max == 14 {
			constrained = true
		}
	}
	if !constrained {
		t.Fatal("a stale client DELETED a date-constrained watch")
	}
}

func TestSubscribeBothKeysRejected(t *testing.T) {
	h, _, _ := testAPI(t)
	body := fmt.Sprintf(`{"endpoint":%q,"p256dh":"cGsx","auth":"YXV0aA","watches":[%s],"topics":["rf_LON-TYO_rt_C"]}`,
		endpoint, unboundedRT)
	rec, decoded := do(t, h, "POST", "/subscribe", body, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("both keys: %d, want 400 (%v)", rec.Code, decoded)
	}
	// Neither key is also a 400 (an empty body must not silently wipe watches).
	rec, _ = do(t, h, "POST", "/subscribe",
		fmt.Sprintf(`{"endpoint":%q,"p256dh":"cGsx","auth":"YXV0aA"}`, endpoint), nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("neither key: %d, want 400", rec.Code)
	}
}

// TestValidation walks the §6 table: one reject per rule, plus the accepts.
func TestValidation(t *testing.T) {
	h, _, _ := testAPI(t)
	cases := []struct {
		name, watch, want string
	}{
		{"bad route", `{"route":"LONDON-TYO","kind":"rt","cabins":["C"]}`, "route"},
		{"bad kind", `{"route":"LON-TYO","kind":"xx","cabins":["C"]}`, "kind"},
		{"no cabins", `{"route":"LON-TYO","kind":"rt","cabins":[]}`, "cabin"},
		{"bad cabin", `{"route":"LON-TYO","kind":"rt","cabins":["Z"]}`, "cabin"},
		{"ret on ow", `{"route":"LON-TYO","kind":"ow","cabins":["C"],"ret":{"from":"2026-10-01"}}`, "one-way"},
		{"nights on ow", `{"route":"LON-TYO","kind":"ow","cabins":["C"],"nights":{"min":1,"max":5}}`, "one-way"},
		{"unreal date", `{"route":"LON-TYO","kind":"rt","cabins":["C"],"out":{"from":"2026-02-31","to":"2026-03-05"}}`, "real date"},
		{"from after to", `{"route":"LON-TYO","kind":"rt","cabins":["C"],"out":{"from":"2026-10-20","to":"2026-10-01"}}`, "starts after"},
		{"range > 180 days", `{"route":"LON-TYO","kind":"rt","cabins":["C"],"out":{"from":"2026-08-01","to":"2027-06-01"}}`, "180 days"},
		{"nights out of range", `{"route":"LON-TYO","kind":"rt","cabins":["C"],"nights":{"min":1,"max":61}}`, "nights"},
		{"impossible (EC-4)", `{"route":"LON-TYO","kind":"rt","cabins":["C"],
		   "out":{"from":"2026-10-01","to":"2026-10-20"},"ret":{"from":"2026-10-01","to":"2026-10-05"},
		   "nights":{"min":7,"max":14}}`, "ends before"},
		{"past range (EC-6)", `{"route":"LON-TYO","kind":"ow","cabins":["C"],"out":{"from":"2026-05-01","to":"2026-06-01"}}`, "past"},
		{"absurdly far out", `{"route":"LON-TYO","kind":"ow","cabins":["C"],"out":{"from":"2030-01-01","to":"2030-02-01"}}`, "far in the future"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, body := do(t, h, "POST", "/subscribe", subscribeBody(tc.watch), nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%v)", rec.Code, body)
			}
			msg, _ := body["error"].(string)
			if !strings.Contains(msg, tc.want) {
				t.Errorf("error %q must mention %q", msg, tc.want)
			}
		})
	}

	// Accepts, including the one-day grace: "yesterday" (UTC) is still fine,
	// because the browser's todayIndex() is a LOCAL day and can be ahead of us.
	yesterday := apiNow.UTC().AddDate(0, 0, -1).Format("2006-01-02")
	for _, watch := range []string{
		unboundedRT,
		constrainedW,
		`{"route":"XXX-YYY","kind":"ow","cabins":["M"]}`, // unknown route: grammar only
		fmt.Sprintf(`{"route":"LON-TYO","kind":"ow","cabins":["C"],"out":{"from":%q,"to":%q}}`, yesterday, yesterday),
	} {
		rec, body := do(t, h, "POST", "/subscribe", subscribeBody(watch), nil)
		if rec.Code != http.StatusOK {
			t.Errorf("watch %s must be accepted, got %d: %v", watch, rec.Code, body)
		}
	}
}

func TestTwentyOneWatchesRejected(t *testing.T) {
	h, _, _ := testAPI(t)
	var watches []string
	for i := range alertstore.MaxWatchesPerSub + 1 {
		route := fmt.Sprintf("%s-TYO", string([]byte{byte('A' + i/26), byte('A' + i%26), 'X'}))
		watches = append(watches, fmt.Sprintf(`{"route":%q,"kind":"ow","cabins":["C"]}`, route))
	}
	rec, body := do(t, h, "POST", "/subscribe", subscribeBody(strings.Join(watches, ",")), nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("21 watches: %d, want 400 (%v)", rec.Code, body)
	}
	rec, _ = do(t, h, "POST", "/subscribe",
		subscribeBody(strings.Join(watches[:alertstore.MaxWatchesPerSub], ",")), nil)
	if rec.Code != http.StatusOK {
		t.Errorf("exactly 20 watches: %d, want 200", rec.Code)
	}
}

func TestStatusReflectsHorizon(t *testing.T) {
	store, err := alertstore.Open(alertstore.Options{
		Path: filepath.Join(t.TempDir(), "subs.json"), Debounce: time.Millisecond,
		Now: func() time.Time { return apiNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	today := int(apiNow.UTC().Unix() / 86400)
	srv := New(Config{
		Store: store, Now: func() time.Time { return apiNow },
		Horizon: func() (int, int, map[string]bool) {
			return today, today + 300, map[string]bool{"LON-TYO": true, "TYO-LON": true, "ANU-SKB": true}
		},
	})
	h := srv.Handler()

	body := fmt.Sprintf(`{"endpoint":%q,"p256dh":"cGsx","auth":"YXV0aA","watches":[
	  {"route":"LON-TYO","kind":"rt","cabins":["C"]},
	  {"route":"ANU-SKB","kind":"rt","cabins":["C"]},
	  {"route":"XXX-YYY","kind":"ow","cabins":["C"]}]}`, endpoint)
	rec, decoded := do(t, h, "POST", "/subscribe", body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("subscribe: %d %v", rec.Code, decoded)
	}
	want := map[string]string{
		"LON-TYO": alertstore.StatusActive,
		"ANU-SKB": alertstore.StatusNoReturn,     // EC-7: no reverse route
		"XXX-YYY": alertstore.StatusUnknownRoute, // EC-11: route not in the data
	}
	for _, w := range watchesOf(t, decoded) {
		route := w["route"].(string)
		if w["status"] != want[route] {
			t.Errorf("%s status = %v, want %v", route, w["status"], want[route])
		}
	}
}

func TestUnsubscribeAndBodyCap(t *testing.T) {
	h, store, _ := testAPI(t)
	if _, body := do(t, h, "POST", "/subscribe", subscribeBody(unboundedRT), nil); body["ok"] != true {
		t.Fatal("setup failed")
	}
	rec, _ := do(t, h, "POST", "/unsubscribe", fmt.Sprintf(`{"endpoint":%q}`, endpoint), nil)
	if rec.Code != http.StatusOK || store.Count() != 0 {
		t.Errorf("unsubscribe: %d, %d subs left", rec.Code, store.Count())
	}

	// 32KB cap (§1.3).
	huge := fmt.Sprintf(`{"endpoint":%q,"p256dh":%q,"auth":"YXV0aA","watches":[]}`,
		endpoint, strings.Repeat("A", 40<<10))
	rec, _ = do(t, h, "POST", "/subscribe", huge, nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: %d, want 413", rec.Code)
	}
	// ...and a 20-watch body (~4KB) fits comfortably.
	var watches []string
	for i := range alertstore.MaxWatchesPerSub {
		route := fmt.Sprintf("%s-TYO", string([]byte{byte('A' + i/26), byte('A' + i%26), 'X'}))
		watches = append(watches, fmt.Sprintf(
			`{"route":%q,"kind":"rt","cabins":["M","W","C","F"],"out":{"from":"2026-10-01","to":"2026-10-20"},"ret":{"from":"2026-10-10","to":"2026-10-31"},"nights":{"min":7,"max":14}}`, route))
	}
	rec, body := do(t, h, "POST", "/subscribe", subscribeBody(strings.Join(watches, ",")), nil)
	if rec.Code != http.StatusOK {
		t.Errorf("a full 20-watch body must fit: %d %v", rec.Code, body)
	}
}

func TestCORS(t *testing.T) {
	h, _, _ := testAPI(t)
	for _, path := range []string{"/subscribe", "/watches", "/test"} {
		rec, _ := do(t, h, "OPTIONS", path, "", map[string]string{
			"Origin": origin, "Access-Control-Request-Method": "POST",
		})
		if rec.Code != http.StatusNoContent {
			t.Errorf("%s preflight = %d", path, rec.Code)
		}
		if rec.Header().Get("Access-Control-Allow-Origin") != origin {
			t.Errorf("%s ACAO = %q", path, rec.Header().Get("Access-Control-Allow-Origin"))
		}
	}
	for _, dev := range []string{"http://127.0.0.1:8080", "http://localhost:3000"} {
		rec, _ := do(t, h, "OPTIONS", "/subscribe", "", map[string]string{"Origin": dev})
		if rec.Header().Get("Access-Control-Allow-Origin") != dev {
			t.Errorf("dev origin %s rejected", dev)
		}
	}
	for _, bad := range []string{"https://evil.example", "https://rewardflights.lucy.sh.evil.example", "null"} {
		rec, _ := do(t, h, "OPTIONS", "/subscribe", "", map[string]string{"Origin": bad})
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("origin %s must not be allowed, got %q", bad, got)
		}
	}
}

func TestRateLimit(t *testing.T) {
	store, err := alertstore.Open(alertstore.Options{
		Path: filepath.Join(t.TempDir(), "subs.json"), Debounce: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := apiNow
	h := New(Config{Store: store, RatePerMin: 60, Burst: 20, Now: func() time.Time { return now }}).Handler()
	headers := map[string]string{"CF-Connecting-IP": "203.0.113.7"}

	for i := range 20 {
		if rec, _ := do(t, h, "GET", "/healthz", "", headers); rec.Code != http.StatusOK {
			t.Fatalf("request %d: %d", i+1, rec.Code)
		}
	}
	rec, _ := do(t, h, "GET", "/healthz", "", headers)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("21st request: %d, want 429", rec.Code)
	}
	if retry, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || retry < 1 {
		t.Errorf("Retry-After = %q", rec.Header().Get("Retry-After"))
	}
	if rec, _ := do(t, h, "GET", "/healthz", "", map[string]string{"CF-Connecting-IP": "198.51.100.9"}); rec.Code != http.StatusOK {
		t.Errorf("a different IP has its own bucket: %d", rec.Code)
	}
	now = now.Add(time.Second)
	if rec, _ := do(t, h, "GET", "/healthz", "", headers); rec.Code != http.StatusOK {
		t.Errorf("after refill: %d", rec.Code)
	}
}

// --- POST /test -----------------------------------------------------------

type testPushServer struct {
	*httptest.Server
	status int
	sends  []capturedSend
}

type capturedSend struct {
	path    string
	headers http.Header
	body    []byte
}

func newPushServer(t *testing.T) *testPushServer {
	t.Helper()
	ps := &testPushServer{}
	ps.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ps.sends = append(ps.sends, capturedSend{path: r.URL.Path, headers: r.Header.Clone(), body: body})
		status := ps.status
		if status == 0 {
			status = http.StatusCreated
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(ps.Close)
	return ps
}

type rewriteTransport struct{ host string }

func (rt *rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = rt.host
	return http.DefaultTransport.RoundTrip(clone)
}

func testAPIWithPush(t *testing.T, ps *testPushServer) (http.Handler, *alertstore.Store, *time.Time) {
	t.Helper()
	store, err := alertstore.Open(alertstore.Options{
		Path: filepath.Join(t.TempDir(), "subs.json"), Debounce: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	vapid, err := webpush.NewVapid(key, "mailto:alerts@rewardflights.lucy.sh")
	if err != nil {
		t.Fatal(err)
	}
	now := apiNow
	srv := New(Config{
		Store: store, RatePerMin: 600, Burst: 200, TestPerHour: 5,
		Sender: &webpush.Sender{
			Client: &http.Client{Transport: &rewriteTransport{host: strings.TrimPrefix(ps.URL, "http://")}},
			Vapid:  vapid,
		},
		Now: func() time.Time { return now },
	})
	return srv.Handler(), store, &now
}

func subscribeReal(t *testing.T, store *alertstore.Store, endpoint string) {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 16)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(alertstore.Subscription{
		Endpoint: endpoint,
		P256dh:   base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
		Auth:     base64.RawURLEncoding.EncodeToString(secret),
		Watches:  []alertstore.Watch{{Route: "LON-TYO", Kind: "rt", Cabins: []string{"C"}}},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTestEndpoint(t *testing.T) {
	ps := newPushServer(t)
	h, store, _ := testAPIWithPush(t, ps)

	// Unknown subscription -> 404, and NOTHING is sent.
	rec, body := do(t, h, "POST", "/test", `{"endpoint":"https://fcm.googleapis.com/fcm/send/stranger"}`, nil)
	if rec.Code != http.StatusNotFound || body["error"] != "unknown subscription" {
		t.Fatalf("unknown: %d %v", rec.Code, body)
	}
	if len(ps.sends) != 0 {
		t.Fatal("must not push to an endpoint that is not ours")
	}

	subscribeReal(t, store, endpoint)
	rec, body = do(t, h, "POST", "/test", fmt.Sprintf(`{"endpoint":%q}`, endpoint), nil)
	if rec.Code != http.StatusOK || body["ok"] != true {
		t.Fatalf("test: %d %v", rec.Code, body)
	}
	if len(ps.sends) != 1 {
		t.Fatalf("sent %d, want exactly 1", len(ps.sends))
	}
	send := ps.sends[0]
	if send.headers.Get("Content-Encoding") != "aes128gcm" || send.headers.Get("TTL") != "86400" {
		t.Errorf("headers = %v", send.headers)
	}
	if !strings.HasPrefix(send.headers.Get("Authorization"), "vapid t=") {
		t.Error("missing VAPID auth")
	}
	if len(send.body) < 21+65+17 || send.body[20] != 65 {
		t.Errorf("not an aes128gcm message (%d bytes)", len(send.body))
	}
}

func TestTestExpiredAndFailed(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		t.Run(fmt.Sprintf("expired-%d", status), func(t *testing.T) {
			ps := newPushServer(t)
			ps.status = status
			h, store, _ := testAPIWithPush(t, ps)
			subscribeReal(t, store, endpoint)

			rec, body := do(t, h, "POST", "/test", fmt.Sprintf(`{"endpoint":%q}`, endpoint), nil)
			if rec.Code != http.StatusGone || body["error"] != "subscription expired" {
				t.Fatalf("%d %v", rec.Code, body)
			}
			if store.Count() != 0 {
				t.Error("an expired subscription must be removed")
			}
		})
	}

	ps := newPushServer(t)
	ps.status = http.StatusInternalServerError
	h, store, _ := testAPIWithPush(t, ps)
	subscribeReal(t, store, endpoint)
	rec, _ := do(t, h, "POST", "/test", fmt.Sprintf(`{"endpoint":%q}`, endpoint), nil)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("5xx: %d, want 502", rec.Code)
	}
	if store.Count() != 1 {
		t.Error("a transient failure must not cost the subscription")
	}
}

func TestTestRateLimitPerEndpoint(t *testing.T) {
	ps := newPushServer(t)
	h, store, now := testAPIWithPush(t, ps)
	const other = "https://fcm.googleapis.com/fcm/send/second"
	subscribeReal(t, store, endpoint)
	subscribeReal(t, store, other)

	for i := range 5 {
		if rec, _ := do(t, h, "POST", "/test", fmt.Sprintf(`{"endpoint":%q}`, endpoint), nil); rec.Code != http.StatusOK {
			t.Fatalf("test %d: %d", i+1, rec.Code)
		}
	}
	rec, _ := do(t, h, "POST", "/test", fmt.Sprintf(`{"endpoint":%q}`, endpoint), nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("6th: %d, want 429", rec.Code)
	}
	if len(ps.sends) != 5 {
		t.Errorf("a throttled test must not send: %d sends", len(ps.sends))
	}
	if rec, _ := do(t, h, "POST", "/test", fmt.Sprintf(`{"endpoint":%q}`, other), nil); rec.Code != http.StatusOK {
		t.Errorf("a different subscription has its own budget: %d", rec.Code)
	}
	*now = now.Add(time.Hour + time.Second)
	if rec, _ := do(t, h, "POST", "/test", fmt.Sprintf(`{"endpoint":%q}`, endpoint), nil); rec.Code != http.StatusOK {
		t.Errorf("after the window: %d", rec.Code)
	}
}
