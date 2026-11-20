// internal/board/schedule.go
package board

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The three triggers a watch can have (spec §5). They are constants because the
// scheduler, the create route and the CLI all compare against them.
const (
	TriggerCron      = "cron"
	TriggerInterval  = "interval"
	TriggerCondition = "condition"
)

// minInterval is the interval floor (M4 D4). The scheduler ticks every 15s, so a
// watch firing more often than once a minute is a cron expression's job, and a
// sub-minute interval would be a busy loop over a table.
const minInterval = time.Minute

// cronSchedule is one parsed five-field expression as bit sets. Bit i of each set
// means "value i matches": minute 0..59, hour 0..23, day-of-month 1..31, month
// 1..12, day-of-week 0..6 (Sunday = 0).
//
// A `*` field sets every bit, which is what makes the day-field rule fall out of
// ordinary conjunction (M4 D3): when one of day-of-month and day-of-week is `*`,
// only the other constrains; when both are restricted, both must match. Vixie
// cron ORs restricted day fields; this board does not, and the difference is
// pinned by TestCronDayFieldsAreAnAND.
type cronSchedule struct {
	minute uint64
	hour   uint64
	dom    uint64
	month  uint64
	dow    uint64
}

// NextFire is the next time one trigger's schedule fires after from, in seconds.
//
// It is the only schedule computation in the package: create-time validation calls
// it with the current clock and discards the result, and the scheduler calls it to
// advance a watch after a fire or a skip. The plan named two functions
// (ParseSchedule and NextFire); they were the same computation, and two spellings
// of one rule is how the two drift apart.
//
// A condition watch has no schedule of its own — its probe decides — so its next
// fire is `from`. The caller supplies the tick.
func NextFire(trigger, schedule string, from int64) (int64, error) {
	switch trigger {
	case TriggerCron:
		c, err := parseCron(schedule)
		if err != nil {
			return 0, err
		}
		return c.next(from)
	case TriggerInterval:
		d, err := parseInterval(schedule)
		if err != nil {
			return 0, err
		}
		return time.Unix(from, 0).UTC().Add(d).Unix(), nil
	case TriggerCondition:
		if strings.TrimSpace(schedule) != "" {
			return 0, fmt.Errorf("a condition watch has no schedule: its probe decides when it fires, so schedule must be empty")
		}
		return from, nil
	default:
		return 0, fmt.Errorf("trigger %q is not one of %q, %q, %q", trigger, TriggerCron, TriggerInterval, TriggerCondition)
	}
}

// parseInterval reads an interval as a Go duration and applies the floor.
func parseInterval(schedule string) (time.Duration, error) {
	s := strings.TrimSpace(schedule)
	if s == "" {
		return 0, fmt.Errorf("an interval watch needs a schedule, for example 5m or 24h")
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("schedule %q is not a duration (want forms like 90s, 5m, 24h): %w", schedule, err)
	}
	if d < minInterval {
		return 0, fmt.Errorf("schedule %q is below the one-minute floor: the scheduler ticks every 15s and a watch that fires more often than once a minute is a cron expression's job", schedule)
	}
	return d, nil
}

// parseCron parses a five-field expression. It refuses everything it does not
// implement — seconds, L/W/#, @macros — rather than approximating it, because a
// schedule that silently means something else is worse than one that is rejected.
func parseCron(expr string) (*cronSchedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		hint := ""
		if len(fields) == 6 {
			hint = " (seconds are not supported: a watch fires at minute resolution)"
		}
		return nil, fmt.Errorf("cron %q has %d fields, want 5: minute hour day-of-month month day-of-week%s", expr, len(fields), hint)
	}
	var c cronSchedule
	var err error
	if c.minute, err = parseCronField(fields[0], "minute", 0, 59, nil); err != nil {
		return nil, err
	}
	if c.hour, err = parseCronField(fields[1], "hour", 0, 23, nil); err != nil {
		return nil, err
	}
	if c.dom, err = parseCronField(fields[2], "day-of-month", 1, 31, nil); err != nil {
		return nil, err
	}
	if c.month, err = parseCronField(fields[3], "month", 1, 12, monthNames); err != nil {
		return nil, err
	}
	if c.dow, err = parseCronField(fields[4], "day-of-week", 0, 7, dayNames); err != nil {
		return nil, err
	}
	// Cron accepts both 0 and 7 for Sunday; the bit set keeps only 0 so that a
	// lookup by time.Weekday needs no alias.
	if c.dow&(1<<7) != 0 {
		c.dow = c.dow&^(1<<7) | 1<<0
	}
	return &c, nil
}

var (
	monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
		"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
	dayNames = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}
)

// parseCronField expands one field into its bit set: `*`, a number, a range
// `a-b`, any of those with `/n`, a comma list of those, and a three-letter name
// where names are allowed. Every rejection names the field and the token.
func parseCronField(field, name string, lo, hi int, names map[string]int) (uint64, error) {
	var set uint64
	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return 0, fmt.Errorf("cron %s field %q: empty list entry", name, field)
		}
		rangePart, step := part, 1
		if i := strings.Index(part, "/"); i >= 0 {
			rangePart = strings.TrimSpace(part[:i])
			n, err := strconv.Atoi(strings.TrimSpace(part[i+1:]))
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("cron %s field %q: step %q is not a positive number", name, part, part[i+1:])
			}
			step = n
		}

		var start, end int
		switch {
		case rangePart == "*":
			start, end = lo, hi
		case strings.Contains(rangePart, "-"):
			a, b, _ := strings.Cut(rangePart, "-")
			var err error
			if start, err = cronValue(a, name, lo, hi, names); err != nil {
				return 0, err
			}
			if end, err = cronValue(b, name, lo, hi, names); err != nil {
				return 0, err
			}
			if start > end {
				return 0, fmt.Errorf("cron %s field %q: range %q runs backwards", name, part, rangePart)
			}
		default:
			if step != 1 {
				return 0, fmt.Errorf("cron %s field %q: a step needs a range or *, as in */5 or 1-30/2", name, part)
			}
			var err error
			if start, err = cronValue(rangePart, name, lo, hi, names); err != nil {
				return 0, err
			}
			end = start
		}
		for v := start; v <= end; v += step {
			set |= 1 << uint(v)
		}
	}
	return set, nil
}

// cronValue reads one end of a range: a name where one is allowed, otherwise a
// number in [lo,hi]. Anything else is refused with the field and the token, so an
// operator can fix the expression without reading this file.
func cronValue(tok, name string, lo, hi int, names map[string]int) (int, error) {
	tok = strings.TrimSpace(tok)
	if names != nil {
		if v, ok := names[strings.ToLower(tok)]; ok {
			return v, nil
		}
	}
	n, err := strconv.Atoi(tok)
	if err != nil {
		return 0, fmt.Errorf("cron %s field: %q is not a number%s", name, tok, nameHint(name))
	}
	if n < lo || n > hi {
		return 0, fmt.Errorf("cron %s field: %d is outside %d..%d%s", name, n, lo, hi, nameHint(name))
	}
	return n, nil
}

func nameHint(name string) string {
	if name == "month" || name == "day-of-week" {
		return " (three-letter names are also accepted)"
	}
	return ""
}

// match reports whether a minute matches every field.
func (c *cronSchedule) match(t time.Time) bool {
	return bitAt(c.minute, t.Minute()) && bitAt(c.hour, t.Hour()) &&
		bitAt(c.dom, t.Day()) && bitAt(c.month, int(t.Month())) &&
		bitAt(c.dow, int(t.Weekday()))
}

func bitAt(set uint64, v int) bool { return set&(1<<uint(v)) != 0 }

// next finds the first matching minute after from, jumping whole months, days and
// hours that cannot match. The straight minute-by-minute search would do for the
// common cases and crawl on `0 0 29 2 *`; the jumps keep the worst case small.
//
// `match` is the one authority on "does this minute fire"; the switch below only
// decides how far it is safe to jump. Keeping the decision in `match` is what makes
// a mutation of the rule observable — when this function duplicated the checks, a
// broken `match` still passed every test.
//
// Five years is the search bound: an expression that names a date the calendar
// never produces (31 February) would otherwise loop forever. The error says so,
// because that expression is a typo, not an empty schedule.
func (c *cronSchedule) next(from int64) (int64, error) {
	t := time.Unix(from, 0).UTC().Truncate(time.Minute).Add(time.Minute)
	deadline := t.AddDate(5, 0, 0)
	for t.Before(deadline) {
		if c.match(t) {
			return t.Unix(), nil
		}
		switch {
		case !bitAt(c.month, int(t.Month())):
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		case !bitAt(c.dom, t.Day()) || !bitAt(c.dow, int(t.Weekday())):
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.UTC)
		case !bitAt(c.hour, t.Hour()):
			t = t.Truncate(time.Hour).Add(time.Hour)
		default:
			t = t.Add(time.Minute)
		}
	}
	return 0, fmt.Errorf("the schedule never matches within five years: it names a date the calendar does not produce (31 February, for example)")
}
