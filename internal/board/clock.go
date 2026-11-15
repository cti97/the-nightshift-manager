package board

import "time"

// Clock is the only source of time in the system. Every timestamp and lease
// is computed from it, server-side; a node's clock is never consulted.
//
// It has two resolutions because the board stores two kinds of time. Now is the
// wall clock a row carries — Unix seconds, because every timestamp in the schema
// is seconds. A probe also measures a round trip, and a healthy one is tens of
// milliseconds, so NowMillis reads the same instant at millisecond resolution;
// measuring a duration with Now would report every fast endpoint as 0ms and
// every slow one in whole seconds. There is deliberately no second time source:
// a duration measured from anything but this Clock could not be audited by a
// test, which is exactly what the probe's latency_ms has to be.
type Clock interface {
	Now() int64
	NowMillis() int64
}

type realClock struct{}

func (realClock) Now() int64 { return time.Now().Unix() }

// NowMillis is the same instant as Now, at millisecond resolution.
func (realClock) NowMillis() int64 { return time.Now().UnixMilli() }

// SystemClock is what production uses.
func SystemClock() Clock { return realClock{} }

// FixedClock is a controllable clock for tests. It stores milliseconds and
// derives seconds from them, so the two readings can never disagree.
type FixedClock struct{ ms int64 }

func NewFixedClock(t int64) *FixedClock { return &FixedClock{ms: t * 1000} }

func (f *FixedClock) Now() int64 { return f.ms / 1000 }

// NowMillis is the sub-second reading, for measuring durations.
func (f *FixedClock) NowMillis() int64 { return f.ms }

// Advance moves the clock by d, in milliseconds. Every existing caller advances
// in whole seconds (leases, heartbeats, report windows), which are exact.
func (f *FixedClock) Advance(d time.Duration) { f.ms += d.Milliseconds() }

// AdvanceMillis moves the clock by an exact number of milliseconds, so a test
// can pin a sub-second measurement without rounding through time.Duration.
func (f *FixedClock) AdvanceMillis(ms int64) { f.ms += ms }

func (f *FixedClock) Set(t int64) { f.ms = t * 1000 }
