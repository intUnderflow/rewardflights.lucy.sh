package alerts

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/webpush"
)

// TestWorkerPublisher drives the real Web Push transport against local
// httptest servers: subscription pull with bearer auth, RFC 8291 body and
// headers on the push POST, dead-subscription pruning on 410, and the
// pull-failure error contract.
func TestWorkerPublisher(t *testing.T) {
	uaKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatal(err)
	}
	p256dh := base64.RawURLEncoding.EncodeToString(uaKey.PublicKey().Bytes())
	authB64 := base64.RawURLEncoding.EncodeToString(auth)

	var pushed struct {
		headers http.Header
		body    []byte
	}
	pushSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			pushed.headers = r.Header.Clone()
			pushed.body, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
		case "/gone":
			w.WriteHeader(http.StatusGone)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer pushSrv.Close()

	var deleted []string
	var gotTopic, gotAuth string
	workerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/subs" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodGet:
			gotTopic = r.URL.Query().Get("topic")
			gotAuth = r.Header.Get("Authorization")
			subs := []webpush.Subscription{
				{Endpoint: pushSrv.URL + "/ok", P256dh: p256dh, Auth: authB64},
				{Endpoint: pushSrv.URL + "/gone", P256dh: p256dh, Auth: authB64},
			}
			json.NewEncoder(w).Encode(subs)
		case http.MethodDelete:
			var req struct {
				Endpoint string `json:"endpoint"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			deleted = append(deleted, req.Endpoint)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer workerSrv.Close()

	vapidKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	vapid, err := webpush.NewVapid(vapidKey, "mailto:alerts@rewardflights.lucy.sh")
	if err != nil {
		t.Fatal(err)
	}
	cap := &capture{}
	publish := workerPublisher(workerSrv.URL, "s3cret", vapid, cap.logf)

	pub := Publication{
		Topic: "rf_LON-TYO_rt_C",
		Title: "Business round trip open: LON ⇄ TYO",
		Body:  "1 new date: Mon 12 Oct",
		URL:   "https://rewardflights.lucy.sh/trip/LON-TYO",
	}
	if err := publish(pub); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if gotTopic != pub.Topic {
		t.Errorf("pulled topic = %q", gotTopic)
	}
	if gotAuth != "Bearer s3cret" {
		t.Errorf("pull auth = %q", gotAuth)
	}

	// The healthy endpoint got a well-formed RFC 8291 push.
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

	// The 410 endpoint was pruned from the store.
	if len(deleted) != 1 || deleted[0] != pushSrv.URL+"/gone" {
		t.Errorf("pruned = %v", deleted)
	}

	// Pull failure -> error (batch stays pending, nothing was attempted).
	brokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer brokenSrv.Close()
	if err := workerPublisher(brokenSrv.URL, "s", vapid, cap.logf)(pub); err == nil {
		t.Error("failed subscription pull must return an error")
	}

	// 404 from the store -> no subscribers -> delivered (nil).
	emptySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer emptySrv.Close()
	if err := workerPublisher(emptySrv.URL, "s", vapid, cap.logf)(pub); err != nil {
		t.Errorf("404 store must count as delivered: %v", err)
	}
}
