package tglink

import (
	"testing"
	"time"

	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/alertstore"
)

func watch(t *testing.T) alertstore.Watch {
	t.Helper()
	w, err := alertstore.Normalize(alertstore.Watch{Route: "LON-TYO", Kind: "rt", Cabins: []string{"C"}})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestCodesAreSingleUseAndExpire(t *testing.T) {
	p := New()
	now := time.Unix(1000, 0)
	p.now = func() time.Time { return now }

	code, err := p.Put([]alertstore.Watch{watch(t)})
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != 22 {
		t.Fatalf("code length %d, want 22", len(code))
	}
	if _, ok := p.Take("not-a-code"); ok {
		t.Fatal("unknown code redeemed")
	}
	got, ok := p.Take(code)
	if !ok || len(got) != 1 || got[0].Route != "LON-TYO" {
		t.Fatalf("take = %v %v", got, ok)
	}
	if _, ok := p.Take(code); ok {
		t.Fatal("code redeemed twice")
	}

	code2, _ := p.Put([]alertstore.Watch{watch(t)})
	now = now.Add(TTL + time.Second)
	if _, ok := p.Take(code2); ok {
		t.Fatal("expired code redeemed")
	}
}

func TestPendingCap(t *testing.T) {
	p := New()
	for i := 0; i < MaxPending; i++ {
		if _, err := p.Put(nil); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if _, err := p.Put(nil); err != ErrFull {
		t.Fatalf("over-cap put: %v, want ErrFull", err)
	}
}
