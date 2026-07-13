package derive

import "testing"

func day(t *testing.T, date string) int {
	t.Helper()
	d, err := parseDay(date)
	if err != nil {
		t.Fatalf("parseDay(%q): %v", date, err)
	}
	return d
}

func TestCabinBitsToNibbles(t *testing.T) {
	cases := []struct {
		cabins []string
		want   byte
	}{
		{nil, '0'},
		{[]string{"M"}, '1'},
		{[]string{"W"}, '2'},
		{[]string{"C"}, '4'},
		{[]string{"F"}, '8'},
		{[]string{"M", "C"}, '5'},
		{[]string{"W", "F"}, 'A'},
		{[]string{"M", "W", "C"}, '7'},
		{[]string{"M", "W", "C", "F"}, 'F'},
		{[]string{"F", "C", "W", "M"}, 'F'}, // order-independent
	}
	epochDay := day(t, "2026-01-01")
	for _, tc := range cases {
		bits := 0
		for _, c := range tc.cabins {
			bits |= CabinBits[c]
		}
		s := NibbleString(1, epochDay, map[int]int{epochDay: bits})
		if s[0] != tc.want {
			t.Errorf("cabins %v: got %q, want %q", tc.cabins, s[0], tc.want)
		}
	}
}

func TestNibbleStringPlacementAndLength(t *testing.T) {
	epochDay := day(t, "2026-01-01")
	s := NibbleString(367, epochDay, map[int]int{
		day(t, "2026-01-01"): 1,  // index 0
		day(t, "2026-03-01"): 4,  // index 31+28 = 59 (2026 not a leap year)
		day(t, "2026-12-30"): 5,  // index 363
		day(t, "2027-01-02"): 8,  // index 366 (crosses the year boundary)
		day(t, "2025-12-31"): 15, // before epoch: ignored, not a crash
		day(t, "2027-01-03"): 15, // past the horizon: ignored
	})
	if len(s) != 367 {
		t.Fatalf("length = %d, want 367", len(s))
	}
	for i, want := range map[int]byte{0: '1', 59: '4', 363: '5', 366: '8', 1: '0', 364: '0'} {
		if s[i] != want {
			t.Errorf("s[%d] = %q, want %q", i, s[i], want)
		}
	}
}

func TestEpoch(t *testing.T) {
	cases := []struct {
		min, max  string
		wantEpoch string
		wantDays  int
	}{
		// Year-boundary dataset: epoch anchors to Jan 1 of the earliest year.
		{"2026-12-30", "2027-01-02", "2026-01-01", 367},
		// Single-day dataset.
		{"2026-07-12", "2026-07-12", "2026-01-01", 193},
		// Leap year: 2028 has Feb 29.
		{"2028-02-01", "2028-03-01", "2028-01-01", 61},
		{"2028-01-01", "2028-12-31", "2028-01-01", 366},
		// Non-leap span into a leap year, crossing Feb 29.
		{"2027-06-15", "2028-03-01", "2027-01-01", 365 + 31 + 29 + 1},
		// Century leap rule sanity: 2100 is NOT a leap year.
		{"2100-01-01", "2100-12-31", "2100-01-01", 365},
	}
	for _, tc := range cases {
		epoch, epochDay, days, err := Epoch(tc.min, tc.max)
		if err != nil {
			t.Fatalf("Epoch(%q, %q): %v", tc.min, tc.max, err)
		}
		if epoch != tc.wantEpoch || days != tc.wantDays {
			t.Errorf("Epoch(%q, %q) = (%q, %d), want (%q, %d)",
				tc.min, tc.max, epoch, days, tc.wantEpoch, tc.wantDays)
		}
		if got := day(t, tc.wantEpoch); epochDay != got {
			t.Errorf("epochDay = %d, want %d", epochDay, got)
		}
	}
}

func TestSeatCodeBuckets(t *testing.T) {
	for n, want := range map[int]int{
		-5: 0, -1: 0, 0: 0, 1: 0, // no evidence of >=2 seats
		2: 1, 3: 2, 4: 3, 5: 3, 9: 3, 100: 3,
	} {
		if got := seatCode(n); got != want {
			t.Errorf("seatCode(%d) = %d, want %d", n, got, want)
		}
	}
}

func TestSeatStringPlacementAndLength(t *testing.T) {
	epochDay := day(t, "2026-01-01")
	s := SeatString(367, epochDay, map[int]int{
		day(t, "2026-01-01"): 0x01, // M code 1 at index 0
		day(t, "2026-03-01"): 0xAB, // both hex chars, uppercase, index 59
		day(t, "2027-01-02"): 0xC0, // F code 3 at the last index (366)
		day(t, "2025-12-31"): 0xFF, // before epoch: ignored, not a crash
		day(t, "2027-01-03"): 0xFF, // past the horizon: ignored
	})
	if len(s) != 2*367 {
		t.Fatalf("length = %d, want %d", len(s), 2*367)
	}
	for idx, want := range map[int]string{0: "01", 59: "AB", 366: "C0", 1: "00", 365: "00"} {
		if got := s[2*idx : 2*idx+2]; got != want {
			t.Errorf("s[day %d] = %q, want %q", idx, got, want)
		}
	}
}

func TestCabinLetters(t *testing.T) {
	for bits, want := range map[int]string{
		0: "", 1: "M", 2: "W", 4: "C", 8: "F",
		5: "MC", 10: "WF", 15: "MWCF",
	} {
		if got := cabinLetters(bits); got != want {
			t.Errorf("cabinLetters(%d) = %q, want %q", bits, got, want)
		}
	}
}

func TestUnixDayCutoff(t *testing.T) {
	// 1770000000 = 2026-02-02T02:40:00Z -> cutoff day is 2026-02-02.
	if got, want := unixDay(1770000000), day(t, "2026-02-02"); got != want {
		t.Errorf("unixDay(1770000000) = %d, want %d", got, want)
	}
	// Midnight boundary is inclusive of its own day.
	if got, want := unixDay(int64(day(t, "2026-07-11"))*86400), day(t, "2026-07-11"); got != want {
		t.Errorf("unixDay(midnight) = %d, want %d", got, want)
	}
}
