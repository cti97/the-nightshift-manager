package board

import "time"

// Clock is the only source of time in the system. Every timestamp and lease
// is computed from it, server-side; a node's clock is never consulted.
type Clock interface{ Now() int64 }

type realClock struct{}

func (realClock) Now() int64 { return time.Now().Unix() }

// SystemClock is what production uses.
func SystemClock() Clock { return realClock{} }

// FixedClock is a controllable clock for tests.
type FixedClock struct{ t int64 }

func NewFixedClock(t int64) *FixedClock { return &FixedClock{t: t} }

func (f *FixedClock) Now() int64              { return f.t }
func (f *FixedClock) Advance(d time.Duration) { f.t += int64(d.Seconds()) }
func (f *FixedClock) Set(t int64)             { f.t = t }
