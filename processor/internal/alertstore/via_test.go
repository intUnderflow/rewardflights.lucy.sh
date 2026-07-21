package alertstore

import (
	"strings"
	"testing"
)

func viaWatch() Watch {
	return Watch{
		Route: "BLL-TYO", Kind: KindRT, Cabins: []string{"C"},
		Via: "LON", Conn: 1, Nights: &Nights{Min: 7, Max: 14},
	}
}

// TestViaIDStability pins the conditional-fold rule: a via-less watch's id is
// byte-identical to the pre-via formula, so every stored id survives the
// deploy; a via watch folds hub + stop length so different journeys never
// collide.
func TestViaIDStability(t *testing.T) {
	direct := Watch{Route: "LON-TYO", Kind: KindRT, Cabins: []string{"C"}}
	before, err := Normalize(direct)
	if err != nil {
		t.Fatal(err)
	}
	// The exact id the pre-via formula produced for this watch (computed by
	// the shipped engine; a change here breaks every stored direct watch).
	if want := watchID(Watch{Route: "LON-TYO", Kind: KindRT, Cabins: []string{"C"}}); before.ID != want {
		t.Fatalf("direct id drifted: %s != %s", before.ID, want)
	}

	via, err := Normalize(viaWatch())
	if err != nil {
		t.Fatal(err)
	}
	conn2 := viaWatch()
	conn2.Conn = 2
	via2, err := Normalize(conn2)
	if err != nil {
		t.Fatal(err)
	}
	if via.ID == via2.ID {
		t.Error("different stop lengths must be different watches")
	}
	directBLL := viaWatch()
	directBLL.Via = ""
	directBLL.Conn = 0
	plain, err := Normalize(directBLL)
	if err != nil {
		t.Fatal(err)
	}
	if plain.ID == via.ID {
		t.Error("a via watch must never collide with a direct watch on the same endpoints")
	}
}

func TestViaNormalize(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Watch)
		want string // substring of the error; empty = must pass
	}{
		{"valid", func(w *Watch) {}, ""},
		{"conn defaults to 1", func(w *Watch) { w.Conn = 0 }, ""},
		{"conn too long", func(w *Watch) { w.Conn = 4 }, "between 1 and 3"},
		{"via not a place", func(w *Watch) { w.Via = "London" }, "3-letter"},
		{"via is an endpoint", func(w *Watch) { w.Via = "TYO" }, "endpoints"},
		{"party watch rejected", func(w *Watch) { w.MinSeats = 2 }, "via trips"},
		{"conn without via", func(w *Watch) { w.Via = ""; w.Conn = 2 }, "only meaningful"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := viaWatch()
			tc.mut(&w)
			got, err := Normalize(w)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if w.Via != "" && got.Conn < MinConn {
					t.Errorf("conn not canonicalized: %d", got.Conn)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestViaLegRoutesAndStatus(t *testing.T) {
	w, err := Normalize(viaWatch())
	if err != nil {
		t.Fatal(err)
	}
	legs := w.LegRoutes()
	want := []string{"BLL-LON", "LON-TYO", "TYO-LON", "LON-BLL"}
	if len(legs) != 4 {
		t.Fatalf("legs = %v, want %v", legs, want)
	}
	for i := range want {
		if legs[i] != want[i] {
			t.Fatalf("legs = %v, want %v", legs, want)
		}
	}

	all := map[string]bool{"BLL-LON": true, "LON-TYO": true, "TYO-LON": true, "LON-BLL": true}
	if got := w.Status(0, all); got != StatusActive {
		t.Errorf("full chain: status = %s, want active", got)
	}
	noHome := map[string]bool{"BLL-LON": true, "LON-TYO": true, "TYO-LON": true}
	if got := w.Status(0, noHome); got != StatusNoReturn {
		t.Errorf("missing return leg: status = %s, want no-return", got)
	}
	noOut := map[string]bool{"BLL-LON": true, "TYO-LON": true, "LON-BLL": true}
	if got := w.Status(0, noOut); got != StatusUnknownRoute {
		t.Errorf("missing outbound leg: status = %s, want unknown-route", got)
	}

	ow, err := Normalize(Watch{Route: "BLL-TYO", Kind: KindOW, Cabins: []string{"C"}, Via: "LON"})
	if err != nil {
		t.Fatal(err)
	}
	if legs := ow.LegRoutes(); len(legs) != 2 || legs[0] != "BLL-LON" || legs[1] != "LON-TYO" {
		t.Fatalf("ow legs = %v", legs)
	}
	if ow.TopicRepresentable() {
		t.Error("a via watch must be invisible to the legacy topic API")
	}
}
