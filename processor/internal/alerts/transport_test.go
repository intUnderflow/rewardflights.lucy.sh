package alerts

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/alertstore"
	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/webpush"
)

// pushKeys mints a subscriber keypair as a browser would hand it to us.
func pushKeys(t *testing.T) (p256dh, auth string) {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 16)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
		base64.RawURLEncoding.EncodeToString(secret)
}

func testVapid(t *testing.T) *webpush.Vapid {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	v, err := webpush.NewVapid(key, "mailto:alerts@rewardflights.lucy.sh")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// rewriteTransport routes every request to a local test server, preserving
// the path — so the sender can use real (allowlisted) push endpoint URLs.
type rewriteTransport struct {
	host string
	base http.RoundTripper
}

func (rt *rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = rt.host
	return rt.base.RoundTrip(clone)
}

// redirectPush points the package's push client at target for this test.
func redirectPush(t *testing.T, target string) {
	t.Helper()
	original := pushClient.Transport
	pushClient.Transport = &rewriteTransport{
		host: strings.TrimPrefix(target, "http://"),
		base: http.DefaultTransport,
	}
	t.Cleanup(func() { pushClient.Transport = original })
}

// TestStorePublisher drives the real Web Push transport with subscribers read
// straight from the local store: a well-formed RFC 8291 body with VAPID auth
// reaches the push service, a 410 removes the subscription from the store
// directly, and a 5xx does not.
func TestStorePublisher(t *testing.T) {
	var pushed struct {
		headers http.Header
		body    []byte
		count   int
	}
	push := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/ok"):
			pushed.headers = r.Header.Clone()
			pushed.body, _ = io.ReadAll(r.Body)
			pushed.count++
			w.WriteHeader(http.StatusCreated)
		case strings.HasSuffix(r.URL.Path, "/gone"):
			w.WriteHeader(http.StatusGone)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer push.Close()
	redirectPush(t, push.URL)

	store, err := alertstore.Open(alertstore.Options{Path: filepath.Join(t.TempDir(), "subs.json")})
	if err != nil {
		t.Fatal(err)
	}
	const (
		okEnd   = "https://fcm.googleapis.com/fcm/send/ok"
		goneEnd = "https://fcm.googleapis.com/fcm/send/gone"
		boomEnd = "https://fcm.googleapis.com/fcm/send/boom"
		topic   = "rf_LON-TYO_rt_C"
	)
	for _, endpoint := range []string{okEnd, goneEnd, boomEnd} {
		p256dh, auth := pushKeys(t)
		if _, err := store.Upsert(alertstore.Subscription{
			Endpoint: endpoint, P256dh: p256dh, Auth: auth, Topics: []string{topic},
		}); err != nil {
			t.Fatalf("upsert %s: %v", endpoint, err)
		}
	}

	cap := &capture{}
	publish := storePublisher(store, testVapid(t), cap.logf)
	err = publish(Publication{
		Topic: topic,
		Title: "Business round trip open: LON ⇄ TYO",
		Body:  "1 new date: Mon 12 Oct",
		URL:   "https://rewardflights.lucy.sh/trip/LON-TYO",
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	if pushed.count != 1 {
		t.Fatalf("healthy endpoint got %d pushes, want 1", pushed.count)
	}
	h := pushed.headers
	if h.Get("TTL") != "86400" || h.Get("Content-Encoding") != "aes128gcm" ||
		h.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("push headers = %v", h)
	}
	if !strings.HasPrefix(h.Get("Authorization"), "vapid t=") || !strings.Contains(h.Get("Authorization"), ", k=") {
		t.Errorf("vapid header = %q", h.Get("Authorization"))
	}
	if len(pushed.body) < 21+65+17 || pushed.body[20] != 65 {
		t.Errorf("push body is not an aes128gcm message (len %d)", len(pushed.body))
	}

	// 410 -> removed from the store. 5xx -> retained (transient failure must
	// not cost a subscription).
	if store.Topics(goneEnd) != nil {
		t.Error("410 endpoint must be removed from the store")
	}
	if store.Topics(boomEnd) == nil {
		t.Error("5xx endpoint must be retained")
	}
	if store.Topics(okEnd) == nil {
		t.Error("healthy endpoint must be retained")
	}
	if got := store.Count(); got != 2 {
		t.Errorf("store count = %d, want 2", got)
	}
	if !strings.Contains(strings.Join(cap.logs, "\n"), "WARN alert-push-failed") {
		t.Errorf("5xx endpoint must log a failure warning: %v", cap.logs)
	}

	// A topic nobody subscribed to is a no-op success (never re-batched).
	if err := publish(Publication{Topic: "rf_LON-JFK_ow_M", Title: "t", Body: "b"}); err != nil {
		t.Errorf("empty topic must succeed: %v", err)
	}
}

// TestStorePublisherEndToEndThroughWatcher proves the detection layer and the
// store-backed sender are wired together: a real availability transition
// delivers an encrypted push to the subscriber of exactly that topic.
func TestStorePublisherEndToEndThroughWatcher(t *testing.T) {
	var got struct {
		count int
		paths []string
	}
	push := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.count++
		got.paths = append(got.paths, r.URL.Path)
		w.WriteHeader(http.StatusCreated)
	}))
	defer push.Close()
	redirectPush(t, push.URL)

	store, err := alertstore.Open(alertstore.Options{Path: filepath.Join(t.TempDir(), "subs.json")})
	if err != nil {
		t.Fatal(err)
	}
	p256dh, auth := pushKeys(t)
	if _, err := store.Upsert(alertstore.Subscription{
		Endpoint: "https://fcm.googleapis.com/fcm/send/abc",
		P256dh:   p256dh, Auth: auth,
		Topics: []string{"rf_LON-TYO_ow_C"}, // subscribed to Business one-ways only
	}); err != nil {
		t.Fatal(err)
	}

	w, err := NewWatcher(Config{
		Store: store, Window: 30,
		VapidKeyPath: writeVapidKey(t),
		VapidSubject: "mailto:alerts@rewardflights.lucy.sh",
		Logf:         func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	w.Baseline(rawBundle(t, "2026-01-01", t0, ba(map[string]string{"LON-TYO": "00000000"}), nil))
	// Business opens on Jan 3 (the subscribed topic) AND Economy on Jan 4
	// (nobody subscribed) -> exactly one push.
	w.Cycle(rawBundle(t, "2026-01-01", t0+600, ba(map[string]string{"LON-TYO": "00410000"}), nil))

	if got.count != 1 {
		t.Fatalf("delivered %d pushes (%v), want exactly 1 (the subscribed topic)", got.count, got.paths)
	}
	if got.paths[0] != "/fcm/send/abc" {
		t.Errorf("pushed to %q", got.paths[0])
	}
}

// writeVapidKey writes a fresh VAPID key to a temp file and returns its path.
func writeVapidKey(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	scalar := make([]byte, 32)
	key.D.FillBytes(scalar)
	path := filepath.Join(t.TempDir(), "vapid.key")
	if err := os.WriteFile(path, []byte(base64.RawURLEncoding.EncodeToString(scalar)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
