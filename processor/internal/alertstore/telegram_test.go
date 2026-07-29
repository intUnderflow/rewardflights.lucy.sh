package alertstore

import (
	"path/filepath"
	"testing"
	"time"
)

func tgStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(Options{Path: filepath.Join(t.TempDir(), "subs.json"), Debounce: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUpsertTelegramMergesAndDedupes(t *testing.T) {
	s := tgStore(t)
	w1 := Watch{Route: "LON-TYO", Kind: KindRT, Cabins: []string{"C"}}
	w2 := Watch{Route: "DUB-BOS", Kind: KindOW, Cabins: []string{"M"}}

	got, err := s.UpsertTelegram(42, []Watch{w1})
	if err != nil || len(got) != 1 {
		t.Fatalf("first link: %v %v", got, err)
	}
	// A second /start ADDS — and the same watch twice stays one watch.
	got, err = s.UpsertTelegram(42, []Watch{w1, w2})
	if err != nil || len(got) != 2 {
		t.Fatalf("merge: %v %v", got, err)
	}
	if s.Count() != 1 {
		t.Fatalf("subs = %d, want one chat", s.Count())
	}
	if ws := s.Watches("telegram:42"); len(ws) != 2 {
		t.Fatalf("watches = %d", len(ws))
	}
}

func TestTelegramSubSurvivesReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subs.json")
	s, err := Open(Options{Path: path, Debounce: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertTelegram(7, []Watch{{Route: "LON-TYO", Kind: KindOW, Cabins: []string{"F"}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(Options{Path: path, Debounce: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if ws := s2.Watches("telegram:7"); len(ws) != 1 || ws[0].Route != "LON-TYO" {
		t.Fatalf("reload lost the telegram sub: %v", ws)
	}
}

func TestPublicUpsertRejectsTelegramEndpoints(t *testing.T) {
	s := tgStore(t)
	// The public subscribe path must never accept a guessable chat id.
	_, err := s.Upsert(Subscription{Endpoint: "telegram:42", P256dh: "cGsx", Auth: "YXV0aA",
		Watches: []Watch{{Route: "LON-TYO", Kind: KindOW, Cabins: []string{"C"}}}})
	if err == nil {
		t.Fatal("public Upsert accepted a telegram endpoint")
	}
}

func TestWatchSummary(t *testing.T) {
	w, err := Normalize(Watch{Route: "BLL-TYO", Kind: KindRT, Cabins: []string{"C"},
		Via: "LON", Conn: 2, Nights: &Nights{Min: 7, Max: 14}, Airline: ""})
	if err != nil {
		t.Fatal(err)
	}
	want := "BLL ⇄ TYO · Business · any date · 7–14 nights · via LON (≤2n stop)"
	if got := w.Summary(); got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
	scoped, err := Normalize(Watch{Route: "LON-DUB", Kind: KindOW, Cabins: []string{"M", "W", "C", "F"}, Airline: "EI"})
	if err != nil {
		t.Fatal(err)
	}
	if got := scoped.Summary(); got != "LON → DUB · any cabin · any date · EI only" {
		t.Fatalf("scoped summary = %q", got)
	}
}
