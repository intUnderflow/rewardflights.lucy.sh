package alertstore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	endpointA = "https://fcm.googleapis.com/fcm/send/aaa"
	endpointB = "https://web.push.apple.com/QWERTY"
	endpointC = "https://updates.push.services.mozilla.com/wpush/v2/zzz"
)

func openStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(Options{Path: path, Debounce: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func sub(endpoint string, topics ...string) Subscription {
	return Subscription{Endpoint: endpoint, P256dh: "cGsx", Auth: "YXV0aA", Topics: topics}
}

func TestUpsertGetTopicsCount(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "subs.json"))

	topics, err := s.Upsert(sub(endpointA, "rf_LON-TYO_rt_C", "rf_LON-TYO_ow_C", "rf_LON-TYO_rt_C"))
	if err != nil {
		t.Fatal(err)
	}
	// Deduplicated and sorted.
	if want := []string{"rf_LON-TYO_ow_C", "rf_LON-TYO_rt_C"}; !slices.Equal(topics, want) {
		t.Errorf("stored topics = %v, want %v", topics, want)
	}
	if _, err := s.Upsert(sub(endpointB, "rf_LON-TYO_rt_C")); err != nil {
		t.Fatal(err)
	}

	if got := s.Get("rf_LON-TYO_rt_C"); len(got) != 2 {
		t.Errorf("topic index: %d subs, want 2", len(got))
	}
	if got := s.Get("rf_LON-TYO_ow_C"); len(got) != 1 || got[0].Endpoint != endpointA {
		t.Errorf("topic index: %v", got)
	}
	if got := s.Get("rf_NYC-LON_ow_M"); len(got) != 0 {
		t.Errorf("unknown topic must be empty, got %v", got)
	}
	if got := s.Count(); got != 2 {
		t.Errorf("count = %d, want 2", got)
	}
	if got := s.ActiveTopics(); !slices.Equal(got, []string{"rf_LON-TYO_ow_C", "rf_LON-TYO_rt_C"}) {
		t.Errorf("active topics = %v", got)
	}

	// Upsert REPLACES the topic set (not a merge) and reindexes.
	if _, err := s.Upsert(sub(endpointA, "rf_NYC-LON_ow_M")); err != nil {
		t.Fatal(err)
	}
	if got := s.Topics(endpointA); !slices.Equal(got, []string{"rf_NYC-LON_ow_M"}) {
		t.Errorf("after replace, topics = %v", got)
	}
	if got := s.Get("rf_LON-TYO_ow_C"); len(got) != 0 {
		t.Errorf("stale topic index entry survived: %v", got)
	}
	if got := s.Get("rf_LON-TYO_rt_C"); len(got) != 1 || got[0].Endpoint != endpointB {
		t.Errorf("rt index after replace = %v", got)
	}
	if got := s.Count(); got != 2 {
		t.Errorf("replace must not duplicate: count = %d", got)
	}

	// Remove is idempotent and cleans the index.
	s.Remove(endpointA)
	s.Remove(endpointA)
	if s.Topics(endpointA) != nil {
		t.Error("removed endpoint still present")
	}
	if got := s.Get("rf_NYC-LON_ow_M"); len(got) != 0 {
		t.Errorf("index entry survived removal: %v", got)
	}
	if got := s.Count(); got != 1 {
		t.Errorf("count after removal = %d, want 1", got)
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "subs.json")
	s := openStore(t, path)
	if _, err := s.Upsert(sub(endpointA, "rf_LON-TYO_rt_C")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Upsert(sub(endpointC, "rf_LON-TYO_rt_C", "rf_LON-SIN_ow_F")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil { // always flushes on shutdown
		t.Fatal(err)
	}

	// On-disk shape: schema + sorted subs, one atomic file, no .tmp left over.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file storeFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("unparseable store file: %v\n%s", err, raw)
	}
	if file.Schema != 1 || len(file.Subs) != 2 {
		t.Errorf("file = %+v", file)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file left behind")
	}
	if info, err := os.Stat(path); err == nil && info.Mode().Perm() != 0o600 {
		t.Errorf("subscriptions must be owner-only, got %v", info.Mode().Perm())
	}

	// Restart: everything is back, including the topic index.
	s2 := openStore(t, path)
	if got := s2.Count(); got != 2 {
		t.Fatalf("after restart count = %d, want 2", got)
	}
	if got := s2.Get("rf_LON-TYO_rt_C"); len(got) != 2 {
		t.Errorf("after restart topic index = %d subs, want 2", len(got))
	}
	if got := s2.Get("rf_LON-SIN_ow_F"); len(got) != 1 || got[0].Endpoint != endpointC {
		t.Errorf("after restart: %v", got)
	}
	if got := s2.Topics(endpointC); !slices.Equal(got, []string{"rf_LON-SIN_ow_F", "rf_LON-TYO_rt_C"}) {
		t.Errorf("after restart topics = %v", got)
	}
}

func TestDebouncedWriteAndFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subs.json")
	s, err := Open(Options{Path: path, Debounce: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// A burst of upserts must not produce a file write per upsert.
	for i := range 20 {
		if _, err := s.Upsert(sub(fmt.Sprintf("https://fcm.googleapis.com/fcm/send/%d", i), "rf_LON-TYO_ow_M")); err != nil {
			t.Fatal(err)
		}
	}
	// Debounce elapses -> exactly one file, with all 20.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil {
			var file storeFile
			if json.Unmarshal(raw, &file) == nil && len(file.Subs) == 20 {
				return // debounced write landed with the full set
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("debounced write never landed with all 20 subscriptions")
}

func TestCloseFlushesImmediately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subs.json")
	s, err := Open(Options{Path: path, Debounce: time.Hour}) // would never fire on its own
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Upsert(sub(endpointA, "rf_LON-TYO_ow_M")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("write should still be debounced at this point")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Close must flush pending state: %v", err)
	}
	if !strings.Contains(string(raw), endpointA) {
		t.Errorf("flushed file missing the subscription: %s", raw)
	}
}

func TestValidation(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "subs.json"))

	manyTopics := make([]string, MaxTopicsPerSub+1)
	for i := range manyTopics {
		manyTopics[i] = fmt.Sprintf("rf_LO%d-TYO_ow_C", i%10) // distinct-ish, all valid shape
	}
	// Ensure they really are distinct so normalization keeps 61.
	for i := range manyTopics {
		manyTopics[i] = fmt.Sprintf("rf_%s-TYO_ow_C", string([]byte{byte('A' + i/26), byte('A' + i%26), 'X'}))
	}

	cases := []struct {
		name string
		sub  Subscription
		want error
	}{
		{"http endpoint", sub("http://fcm.googleapis.com/x", "rf_LON-TYO_ow_C"), ErrBadEndpoint},
		{"unknown host", sub("https://evil.example.com/x", "rf_LON-TYO_ow_C"), ErrBadEndpoint},
		{"lookalike host", sub("https://fcm.googleapis.com.evil.com/x", "rf_LON-TYO_ow_C"), ErrBadEndpoint},
		{"not a url", sub("::nonsense::", "rf_LON-TYO_ow_C"), ErrBadEndpoint},
		{"empty endpoint", sub("", "rf_LON-TYO_ow_C"), ErrBadEndpoint},
		{"bad topic prefix", sub(endpointA, "notrf_LON-TYO_ow_C"), ErrBadTopic},
		{"bad topic cabin", sub(endpointA, "rf_LON-TYO_ow_Z"), ErrBadTopic},
		{"bad topic type", sub(endpointA, "rf_LON-TYO_xx_C"), ErrBadTopic},
		{"bad topic route", sub(endpointA, "rf_LONDON-TYO_ow_C"), ErrBadTopic},
		{"lowercase route", sub(endpointA, "rf_lon-tyo_ow_C"), ErrBadTopic},
		{"too many topics", sub(endpointA, manyTopics...), ErrTooManyTopics},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Upsert(tc.sub)
			if err == nil {
				t.Fatalf("must be rejected")
			}
			if !strings.Contains(err.Error(), strings.SplitN(tc.want.Error(), ":", 2)[0]) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if got := s.Count(); got != 0 {
		t.Errorf("rejected subscriptions must not be stored (count = %d)", got)
	}

	// Missing keys are rejected too.
	if _, err := s.Upsert(Subscription{Endpoint: endpointA, Topics: []string{"rf_LON-TYO_ow_C"}}); err == nil {
		t.Error("missing p256dh/auth must be rejected")
	}
	// Exactly MaxTopicsPerSub is allowed.
	if _, err := s.Upsert(sub(endpointA, manyTopics[:MaxTopicsPerSub]...)); err != nil {
		t.Errorf("exactly %d topics must be accepted: %v", MaxTopicsPerSub, err)
	}
	// All allowlisted hosts are accepted.
	for _, endpoint := range []string{
		endpointB, endpointC,
		"https://push.services.mozilla.com/wpush/v1/x",
		"https://sg2p.notify.windows.com/w/?token=abc",
		"https://api.push.apple.com/3/device/x",
	} {
		if _, err := s.Upsert(sub(endpoint, "rf_LON-TYO_ow_C")); err != nil {
			t.Errorf("allowlisted host %s rejected: %v", endpoint, err)
		}
	}
}

func TestMaxSubs(t *testing.T) {
	s, err := Open(Options{Path: filepath.Join(t.TempDir(), "subs.json"), MaxSubs: 2, Debounce: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for i := range 2 {
		if _, err := s.Upsert(sub(fmt.Sprintf("https://fcm.googleapis.com/fcm/send/%d", i), "rf_LON-TYO_ow_C")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Upsert(sub("https://fcm.googleapis.com/fcm/send/overflow", "rf_LON-TYO_ow_C")); err != ErrFull {
		t.Errorf("beyond max-subs err = %v, want ErrFull", err)
	}
	// An UPDATE to an existing subscription still works when full.
	if _, err := s.Upsert(sub("https://fcm.googleapis.com/fcm/send/0", "rf_NYC-LON_rt_F")); err != nil {
		t.Errorf("updating an existing sub while full must succeed: %v", err)
	}
	if got := s.Count(); got != 2 {
		t.Errorf("count = %d, want 2", got)
	}
}

func TestCorruptFileStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subs.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := openStore(t, path)
	if got := s.Count(); got != 0 {
		t.Errorf("corrupt file must start empty, got %d", got)
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Error("corrupt file must be preserved aside for forensics")
	}
	// The store is usable afterwards.
	if _, err := s.Upsert(sub(endpointA, "rf_LON-TYO_ow_C")); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDropsInvalidEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subs.json")
	// A hand-edited file smuggling in a non-push host must not be honored.
	raw, err := json.Marshal(storeFile{Schema: 1, Subs: []Subscription{
		sub(endpointA, "rf_LON-TYO_ow_C"),
		sub("https://evil.example.com/x", "rf_LON-TYO_ow_C"),
		sub(endpointB, "rf_BAD"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s := openStore(t, path)
	if got := s.Count(); got != 1 {
		t.Fatalf("count = %d, want 1 (invalid entries dropped)", got)
	}
	if s.Topics(endpointA) == nil {
		t.Error("the valid entry must survive")
	}
}

// TestConcurrentAccess exercises the RWMutex under -race: concurrent
// upserts, removes, and sender reads must not race or corrupt the index.
func TestConcurrentAccess(t *testing.T) {
	s := openStore(t, filepath.Join(t.TempDir(), "subs.json"))
	const workers = 8
	const iterations = 50

	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range iterations {
				endpoint := fmt.Sprintf("https://fcm.googleapis.com/fcm/send/%d-%d", w, i)
				if _, err := s.Upsert(sub(endpoint, "rf_LON-TYO_rt_C", "rf_LON-TYO_ow_C")); err != nil {
					t.Errorf("upsert: %v", err)
					return
				}
				s.Get("rf_LON-TYO_rt_C")
				s.Topics(endpoint)
				s.ActiveTopics()
				s.Count()
				if i%3 == 0 {
					s.Remove(endpoint)
				}
			}
		}(w)
	}
	// Concurrent readers, as the sender would be.
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				for _, got := range s.Get("rf_LON-TYO_rt_C") {
					if got.Endpoint == "" || got.P256dh == "" {
						t.Error("sender read a torn subscription")
						return
					}
				}
			}
		}()
	}
	wg.Wait()

	// Final state is self-consistent: every indexed sub is still stored.
	for _, topic := range s.ActiveTopics() {
		for _, got := range s.Get(topic) {
			if s.Topics(got.Endpoint) == nil {
				t.Errorf("index references a removed endpoint: %s", got.Endpoint)
			}
		}
	}
}
