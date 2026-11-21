// internal/board/schedule_test.go
package board

import (
	"strings"
	"testing"
	"time"
)

// at parses an RFC3339 instant. The tests are written in UTC because the
// scheduler is (M4 D3): a local-time test would prove whichever thing the machine
// running it happens to believe.
func at(t *testing.T, s string) int64 {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return tm.Unix()
}

func stamp(unix int64) string { return isoTime(unix) }

// TestCronFiresAtTheNextMatchingMinute is the core table: every row is a schedule
// an operator would plausibly type, and the answer is the next minute it fires.
func TestCronFiresAtTheNextMatchingMinute(t *testing.T) {
	base := at(t, "2026-11-17T12:00:00Z") // a Tuesday
	cases := []struct {
		expr string
		want string
	}{
		{"*/5 * * * *", "2026-11-17T12:05:00Z"},
		{"0 * * * *", "2026-11-17T13:00:00Z"},
		{"30 2 * * *", "2026-11-18T02:30:00Z"},
		{"0 0 1 * *", "2026-12-01T00:00:00Z"},
		{"0 0 * * 1", "2026-11-23T00:00:00Z"},   // next Monday
		{"0 0 * * mon", "2026-11-23T00:00:00Z"}, // the same, by name
		{"15,45 * * * *", "2026-11-17T12:15:00Z"},
		{"0 0 1 jan,jul *", "2027-01-01T00:00:00Z"},
		{"0 0 29 2 *", "2028-02-29T00:00:00Z"}, // the next leap day
		{"0 0 31 * *", "2026-12-31T00:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			got, err := NextFire(TriggerCron, tc.expr, base)
			if err != nil {
				t.Fatalf("NextFire(%q): %v", tc.expr, err)
			}
			if want := at(t, tc.want); got != want {
				t.Fatalf("NextFire(%q) = %s, want %s", tc.expr, stamp(got), stamp(want))
			}
			if got%60 != 0 {
				t.Fatalf("NextFire(%q) = %s, which is not on a minute boundary: cron is minute resolution", tc.expr, stamp(got))
			}
		})
	}

	// A time inside a minute, not on it: the next fire is still the next whole
	// minute the expression matches.
	got, err := NextFire(TriggerCron, "*/15 9 * * *", at(t, "2026-11-17T09:07:30Z"))
	if err != nil {
		t.Fatalf("NextFire: %v", err)
	}
	if want := at(t, "2026-11-17T09:15:00Z"); got != want {
		t.Fatalf("from 09:07:30 = %s, want %s", stamp(got), stamp(want))
	}
}

// TestCronDayFieldsAreAnAND pins the deliberate difference from Vixie cron (M4
// D3). `0 0 1 * 1` means "the 1st of the month AND a Monday". Under Vixie's OR it
// would fire on 1 December or the nearest Monday; here it is 1 February 2027, the
// first date that is both.
func TestCronDayFieldsAreAnAND(t *testing.T) {
	got, err := NextFire(TriggerCron, "0 0 1 * 1", at(t, "2026-11-17T12:00:00Z"))
	if err != nil {
		t.Fatalf("NextFire: %v", err)
	}
	if want := at(t, "2027-02-01T00:00:00Z"); got != want {
		t.Fatalf("0 0 1 * 1 = %s, want %s (a date that is both the 1st and a Monday)", stamp(got), stamp(want))
	}
}

// TestCronIsUTCAndMinuteResolution: midnights are UTC midnights, and a six-field
// expression is refused with the reason rather than interpreted as five.
func TestCronIsUTCAndMinuteResolution(t *testing.T) {
	got, err := NextFire(TriggerCron, "0 0 * * *", at(t, "2026-11-17T23:30:00Z"))
	if err != nil {
		t.Fatalf("NextFire: %v", err)
	}
	if want := at(t, "2026-11-18T00:00:00Z"); got != want {
		t.Fatalf("midnight = %s, want %s", stamp(got), stamp(want))
	}

	_, err = NextFire(TriggerCron, "0 0 2 * * *", 0)
	if err == nil {
		t.Fatal("a six-field expression was accepted")
	}
	if !strings.Contains(err.Error(), "6 fields") || !strings.Contains(err.Error(), "seconds") {
		t.Fatalf("err = %q, want it to name the field count and the missing seconds support", err)
	}
}

// TestCronRefusesWhatItDoesNotImplement: each row is something a person might type
// and this parser does not implement. The error has to name the field and the
// token, because that is what the operator needs to fix it.
func TestCronRefusesWhatItDoesNotImplement(t *testing.T) {
	cases := []struct{ expr, wantIn string }{
		{"", "want 5"},
		{"0 0 * *", "want 5"},
		{"@daily", "want 5"},
		{"0 0 L * *", "day-of-month"},
		{"0 0 15W * *", "day-of-month"},
		{"0 0 * * 1#2", "day-of-week"},
		{"*/0 * * * *", "positive number"},
		{"60 * * * *", "outside 0..59"},
		{"* 24 * * *", "outside 0..23"},
		{"0 0 32 * *", "outside 1..31"},
		{"0 0 * 13 *", "outside 1..12"},
		{"0 0 * * 8", "outside 0..7"},
		{"5-1 * * * *", "backwards"},
		{"5/2 * * * *", "step needs a range"},
		{"1,,2 * * * *", "empty list entry"},
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			_, err := NextFire(TriggerCron, tc.expr, 0)
			if err == nil {
				t.Fatalf("cron %q was accepted", tc.expr)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Fatalf("err = %q, want it to mention %q", err, tc.wantIn)
			}
		})
	}
}

// TestCronAcrossMonthAndYearBoundaries: a daily schedule crosses them, and a
// day-of-month that the next month does not have is skipped rather than clamped —
// clamping would fire "the 31st" on the 30th, which is not what anyone wrote.
func TestCronAcrossMonthAndYearBoundaries(t *testing.T) {
	cases := []struct{ expr, from, want string }{
		{"0 0 * * *", "2026-01-31T23:59:00Z", "2026-02-01T00:00:00Z"},
		{"0 0 * * *", "2026-12-31T23:59:00Z", "2027-01-01T00:00:00Z"},
		{"0 0 31 * *", "2026-04-01T00:00:00Z", "2026-05-31T00:00:00Z"},
		{"30 2 * * *", "2028-02-28T23:59:00Z", "2028-02-29T02:30:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.expr+" from "+tc.from, func(t *testing.T) {
			got, err := NextFire(TriggerCron, tc.expr, at(t, tc.from))
			if err != nil {
				t.Fatalf("NextFire: %v", err)
			}
			if want := at(t, tc.want); got != want {
				t.Fatalf("= %s, want %s", stamp(got), stamp(want))
			}
		})
	}

	// A date the calendar never produces is a typo, and it has to fail rather
	// than search forever.
	_, err := NextFire(TriggerCron, "0 0 31 2 *", at(t, "2026-01-01T00:00:00Z"))
	if err == nil || !strings.Contains(err.Error(), "never matches") {
		t.Fatalf("31 February = %v, want a never-matches error", err)
	}
}

// TestIntervalScheduleFloor: intervals are Go durations with a one-minute floor,
// and the refusal names the floor rather than just "invalid".
func TestIntervalScheduleFloor(t *testing.T) {
	base := at(t, "2026-11-17T12:00:00Z")
	for _, tc := range []struct {
		dur  string
		want int64
	}{
		{"1m", base + 60},
		{"90s", base + 90},
		{"2h", base + 7200},
		{"24h", base + 86400},
	} {
		got, err := NextFire(TriggerInterval, tc.dur, base)
		if err != nil {
			t.Fatalf("interval %q: %v", tc.dur, err)
		}
		if got != tc.want {
			t.Fatalf("interval %q = %s, want %s", tc.dur, stamp(got), stamp(tc.want))
		}
	}
	for _, tc := range []struct{ dur, wantIn string }{
		{"30s", "one-minute floor"},
		{"0s", "one-minute floor"},
		{"", "needs a schedule"},
		{"10 minutes", "not a duration"},
		{"5", "not a duration"},
	} {
		_, err := NextFire(TriggerInterval, tc.dur, base)
		if err == nil {
			t.Fatalf("interval %q was accepted", tc.dur)
		}
		if !strings.Contains(err.Error(), tc.wantIn) {
			t.Fatalf("interval %q err = %q, want %q", tc.dur, err, tc.wantIn)
		}
	}
}

// TestConditionHasNoSchedule: a condition watch fires when its probe flips, so a
// schedule on it is a misunderstanding worth refusing at create time.
func TestConditionHasNoSchedule(t *testing.T) {
	base := at(t, "2026-11-17T12:00:00Z")
	got, err := NextFire(TriggerCondition, "", base)
	if err != nil {
		t.Fatalf("condition with no schedule: %v", err)
	}
	if got != base {
		t.Fatalf("condition next = %s, want the caller's tick %s", stamp(got), stamp(base))
	}
	if _, err := NextFire(TriggerCondition, "5m", base); err == nil {
		t.Fatal("a condition watch accepted a schedule")
	}
	if _, err := NextFire("hourly", "* * * * *", base); err == nil {
		t.Fatal("an unknown trigger was accepted")
	}
}
