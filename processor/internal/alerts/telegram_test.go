package alerts

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/alertstore"
	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/webpush"
)

// TestTelegramSubscriptionAlerts: a chat-bound subscription rides the whole
// pipeline — indexing, evaluation, batching — and its publication arrives
// addressed to the telegram endpoint.
func TestTelegramSubscriptionAlerts(t *testing.T) {
	store, err := alertstore.Open(alertstore.Options{
		Path: filepath.Join(t.TempDir(), "subs.json"), Debounce: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if _, err := store.UpsertTelegram(4242, []alertstore.Watch{unbounded(alertstore.KindRT, "C")}); err != nil {
		t.Fatal(err)
	}
	cap := &capture{}
	w, err := NewWatcher(Config{Store: store, Publish: cap.publish, Logf: cap.logf})
	if err != nil {
		t.Fatal(err)
	}

	w.Baseline(bundleAt(t, "2026-10-01T09:00", map[string]map[int]string{
		"LON-TYO": {3: "C"}, "TYO-LON": {},
	}))
	w.Cycle(bundleAt(t, "2026-10-01T10:00", map[string]map[int]string{
		"LON-TYO": {3: "C"}, "TYO-LON": {14: "C"},
	}))

	if len(cap.pubs) != 1 {
		t.Fatalf("got %d publications: %v", len(cap.pubs), cap.bodies())
	}
	if cap.subs[0] != "telegram:4242" {
		t.Fatalf("addressed to %q, want the chat endpoint", cap.subs[0])
	}
	if want := "1 new: 3–14 Oct"; cap.pubs[0].Body != want {
		t.Fatalf("body = %q, want %q", cap.pubs[0].Body, want)
	}
}

// TestStorePublisherTelegramDispatch: the built publisher routes telegram
// endpoints to TelegramSend, stamps delivery telemetry on success, and drops
// the subscription when the chat is gone — never bouncing it to Web Push.
func TestStorePublisherTelegramDispatch(t *testing.T) {
	store, err := alertstore.Open(alertstore.Options{
		Path: filepath.Join(t.TempDir(), "subs.json"), Debounce: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if _, err := store.UpsertTelegram(7, []alertstore.Watch{unbounded(alertstore.KindOW, "C")}); err != nil {
		t.Fatal(err)
	}

	var sent []int64
	gone := false
	pub := storePublisher(store, nil, func(chatID int64, p Publication) (bool, error) {
		sent = append(sent, chatID)
		if gone {
			return true, errors.New("blocked")
		}
		return false, nil
	}, func(format string, args ...any) {})

	sub := webpush.Subscription{Endpoint: "telegram:7"}
	if err := pub(sub, Publication{Title: "t", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0] != 7 {
		t.Fatalf("sent = %v", sent)
	}
	if _, lastAck, _, _, ok := store.DeliveryStatus("telegram:7"); !ok || lastAck == 0 {
		t.Fatal("success did not stamp delivery telemetry")
	}

	gone = true
	if err := pub(sub, Publication{Title: "t", Body: "b"}); err != nil {
		t.Fatal("gone must not surface as a retryable error")
	}
	if len(store.Watches("telegram:7")) != 0 {
		t.Fatal("gone chat kept its subscription")
	}

	// No TelegramSend configured: a telegram endpoint is a hard error (the
	// batch is retried after the operator fixes the config), never a push.
	noTG := storePublisher(store, nil, nil, func(string, ...any) {})
	if err := noTG(sub, Publication{}); err == nil || !strings.Contains(err.Error(), "no bot") {
		t.Fatalf("unconfigured dispatch: %v", err)
	}
}
