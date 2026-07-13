package alertapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/alertstore"
)

// TestRepostedWatchesAccepted pins the client's documented read-modify-write
// flow at the HTTP layer: every save path in app.js starts from GET /watches
// and POSTs an edited copy of that list back to /subscribe. The GET response
// carries server-added fields — notably "status", which is NOT part of the
// Watch schema — so the strict decoder must tolerate (and discard) exactly
// the fields the server itself emits, or a save made while any other watch
// exists 400s with "invalid JSON body".
func TestRepostedWatchesAccepted(t *testing.T) {
	h, store, _ := testAPI(t)

	if rec, body := do(t, h, "POST", "/subscribe", subscribeBody(constrainedW), nil); rec.Code != http.StatusOK {
		t.Fatalf("setup: %d %v", rec.Code, body)
	}

	// Fetch the stored list exactly as the client does...
	rec, body := do(t, h, "GET", "/watches?endpoint="+endpoint, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /watches: %d %v", rec.Code, body)
	}
	fetched := watchesOf(t, body)
	if len(fetched) != 1 || fetched[0]["status"] == nil {
		t.Fatalf("GET /watches must echo status (the field under test): %v", fetched)
	}

	// ...append a new watch and POST the whole list back, verbatim.
	fetchedJSON, err := json.Marshal(fetched[0])
	if err != nil {
		t.Fatal(err)
	}
	rec, body = do(t, h, "POST", "/subscribe",
		subscribeBody(string(fetchedJSON)+","+unboundedRT), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-POSTing the server's own GET /watches output must be accepted, got %d: %v", rec.Code, body)
	}
	if got := store.WatchCount(); got != 2 {
		t.Fatalf("store holds %d watches, want 2", got)
	}
	// The echoed status is read-only noise: it must be recomputed, never
	// trusted from the wire.
	for _, w := range watchesOf(t, body) {
		if w["status"] != alertstore.StatusActive {
			t.Errorf("status = %v, want recomputed %q", w["status"], alertstore.StatusActive)
		}
	}

	// The tolerance is a whitelist, not a shrug: an unknown field that the
	// server never emits (a misnamed minSeats, say — note JSON field matching
	// is case-insensitive, so "minseats" is NOT unknown) must still be
	// rejected loudly rather than silently dropping the user's constraint.
	rec, _ = do(t, h, "POST", "/subscribe",
		subscribeBody(`{"route":"LON-TYO","kind":"rt","cabins":["C"],"seats":3}`), nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a genuinely unknown watch field must still 400, got %d", rec.Code)
	}
	rec, _ = do(t, h, "POST", "/subscribe",
		fmt.Sprintf(`{"endpoint":%q,"p256dh":"cGsx","auth":"YXV0aA","watches":[%s],"bogus":1}`, endpoint, unboundedRT), nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("an unknown top-level field must still 400, got %d", rec.Code)
	}
}

// TestMinSeatsValidation walks the minSeats rows: 0/absent and 2..4 are the
// only valid spellings; 1 is rejected so the zero value stays the single
// canonical spelling of "one passenger" (and ids stay content-stable).
func TestMinSeatsValidation(t *testing.T) {
	h, _, _ := testAPI(t)

	watch := func(minSeats int) string {
		return fmt.Sprintf(`{"route":"LON-TYO","kind":"rt","cabins":["C"],"minSeats":%d}`, minSeats)
	}
	rejects := []struct {
		name, body, want string
	}{
		{"negative", watch(-1), "negative"},
		{"explicit 1", watch(1), "default"},
		{"over the cap", watch(5), "between 2 and 4"},
	}
	for _, tc := range rejects {
		t.Run(tc.name, func(t *testing.T) {
			rec, body := do(t, h, "POST", "/subscribe", subscribeBody(tc.body), nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%v)", rec.Code, body)
			}
			if msg, _ := body["error"].(string); !strings.Contains(msg, tc.want) {
				t.Errorf("error %q must mention %q", msg, tc.want)
			}
		})
	}

	for _, n := range []int{2, 3, 4} {
		rec, body := do(t, h, "POST", "/subscribe", subscribeBody(watch(n)), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("minSeats %d must be accepted: %d %v", n, rec.Code, body)
		}
		if got := watchesOf(t, body)[0]["minSeats"]; got != float64(n) {
			t.Errorf("minSeats %d must round-trip, got %v", n, got)
		}
	}
	// 0 is canonical "no constraint": accepted and omitted from the echo.
	rec, body := do(t, h, "POST", "/subscribe", subscribeBody(watch(0)), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("minSeats 0: %d %v", rec.Code, body)
	}
	if _, present := watchesOf(t, body)[0]["minSeats"]; present {
		t.Error("minSeats 0 must be omitted from the wire (omitempty)")
	}
}
