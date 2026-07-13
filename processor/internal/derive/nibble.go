package derive

import (
	"fmt"
	"time"
)

// CabinBits maps a source cabin code to its availability bitmask value.
// One uppercase hex digit (nibble) encodes the OR of a day's cabins.
var CabinBits = map[string]int{"M": 1, "W": 2, "C": 4, "F": 8}

// cabinOrder is the canonical letter order for cabin-set strings ("MWCF").
var cabinOrder = []struct {
	bit    int
	letter byte
}{{1, 'M'}, {2, 'W'}, {4, 'C'}, {8, 'F'}}

const hexDigits = "0123456789ABCDEF"

const secondsPerDay = 86400

// dayNumber returns the number of days from 1970-01-01 to the given civil
// date (dates are pure calendar days; all arithmetic is UTC-anchored).
func dayNumber(year int, month time.Month, day int) int {
	return int(time.Date(year, month, day, 0, 0, 0, 0, time.UTC).Unix() / secondsPerDay)
}

// parseDay converts a "YYYY-MM-DD" string (validated upstream by the source
// walker) into a day number.
func parseDay(date string) (int, error) {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return 0, fmt.Errorf("invalid date %q: %w", date, err)
	}
	return dayNumber(t.Year(), t.Month(), t.Day()), nil
}

// dayDate is the inverse of parseDay: day number -> "YYYY-MM-DD".
func dayDate(day int) string {
	return time.Unix(int64(day)*secondsPerDay, 0).UTC().Format("2006-01-02")
}

// unixDay returns the day number of the UTC calendar date containing the
// given unix timestamp.
func unixDay(unix int64) int {
	if unix < 0 {
		return int(unix/secondsPerDay) - 1 // floor for pre-1970, defensively
	}
	return int(unix / secondsPerDay)
}

// Epoch computes the calendar-anchored nibble-string epoch and length for a
// dataset spanning minDate..maxDate (inclusive, "YYYY-MM-DD"): the epoch is
// January 1 of minDate's year, and days counts epoch through maxDate.
func Epoch(minDate, maxDate string) (epoch string, epochDay, days int, err error) {
	minT, err := time.Parse("2006-01-02", minDate)
	if err != nil {
		return "", 0, 0, fmt.Errorf("invalid min date %q: %w", minDate, err)
	}
	maxDay, err := parseDay(maxDate)
	if err != nil {
		return "", 0, 0, err
	}
	epochDay = dayNumber(minT.Year(), time.January, 1)
	if maxDay < epochDay {
		return "", 0, 0, fmt.Errorf("max date %q precedes epoch of min date %q", maxDate, minDate)
	}
	return fmt.Sprintf("%04d-01-01", minT.Year()), epochDay, maxDay - epochDay + 1, nil
}

// NibbleString renders one route+airline availability string: days uppercase
// hex characters starting at epochDay, one per day, from a day->bits map.
func NibbleString(days, epochDay int, bitsByDay map[int]int) string {
	buf := make([]byte, days)
	for i := range buf {
		buf[i] = '0'
	}
	for day, bits := range bitsByDay {
		i := day - epochDay
		if i >= 0 && i < days {
			buf[i] = hexDigits[bits&0xF]
		}
	}
	return string(buf)
}

// seatCode maps a per-cabin seat count (the MAX across one day's flights of
// one airline) to its 2-bit monotone threshold code: 0 = no evidence of >=2
// seats (count unknown, only 1 seen, or nothing usable — deliberately
// collapsed), 1 = >=2, 2 = >=3, 3 = >=4. A party of N fits iff code >= N-1.
func seatCode(n int) int {
	switch {
	case n >= 4:
		return 3
	case n == 3:
		return 2
	case n == 2:
		return 1
	default:
		return 0
	}
}

// SeatString renders one route+airline seat-threshold string: 2 uppercase hex
// characters (one byte) per day starting at epochDay, from a day -> packed
// byte map. The byte holds one 2-bit seatCode per cabin in MWCF order
// (M = bits 0-1, W = bits 2-3, C = bits 4-5, F = bits 6-7), so the string is
// exactly twice as long as the sibling NibbleString and day d lives at
// s[2d:2d+2]. "00" means no evidence of >=2 seats in any cabin that day.
func SeatString(days, epochDay int, byteByDay map[int]int) string {
	buf := make([]byte, 2*days)
	for i := range buf {
		buf[i] = '0'
	}
	for day, b := range byteByDay {
		i := day - epochDay
		if i >= 0 && i < days {
			buf[2*i] = hexDigits[(b>>4)&0xF]
			buf[2*i+1] = hexDigits[b&0xF]
		}
	}
	return string(buf)
}

// cabinLetters renders a bitmask as cabin letters in canonical MWCF order,
// e.g. 5 -> "MC".
func cabinLetters(bits int) string {
	out := make([]byte, 0, 4)
	for _, c := range cabinOrder {
		if bits&c.bit != 0 {
			out = append(out, c.letter)
		}
	}
	return string(out)
}

// hexBits decodes one nibble character of an availability string; -1 for a
// character that is not an upper- or lowercase hex digit.
func hexBits(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	}
	return -1
}
