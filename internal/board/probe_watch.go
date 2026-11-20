// internal/board/probe_watch.go
package board

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"sync"
	"time"
	"unicode/utf8"
)

// watchProbeTimeout bounds one probe. It is a var rather than a const so a test can
// prove the timeout path without sleeping for half a minute; production never
// writes it.
var watchProbeTimeout = 30 * time.Second

// watchProbeTail is how much of a probe's output is kept. It goes into an event
// payload or a skip reason, and a payload is not a log file: the operator needs the
// last line of a failing command, not a megabyte of it.
const watchProbeTail = 4 << 10

// runWatchProbe runs one watch's command on the manager (M4 D8) and returns the
// observation. The exit code is the answer: 0 is true, anything else is false, and a
// timeout is neither — it is a failed observation, which must not fire and must not
// flip a condition's state.
//
// The environment is PATH and nothing else: a probe inherits no credential from the
// manager's process. The working directory is the manager's, which is the VPS the
// scheduler runs on; a watch that needs to look at a laptop must emit an item with
// the right needs and let placement route it (spec §5).
//
// The output is truncated at a rune boundary rather than a byte boundary. The shell
// tool caps bytes, which is fine for text it hands to a model; this tail is stored
// in JSON, where a half rune becomes U+FFFD in the audit trail.
func runWatchProbe(ctx context.Context, command string, timeout time.Duration) (exit int, tail string, timedOut bool) {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(probeCtx, "/bin/sh", "-c", command)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	// A killed shell can leave a child holding its output pipe, and Wait waits for
	// that pipe before returning — a `sleep` in a probe made a 50ms timeout take two
	// seconds. WaitDelay closes the pipes shortly after the process exits.
	cmd.WaitDelay = 250 * time.Millisecond
	out := &cappedBuffer{max: 2 * watchProbeTail}
	cmd.Stdout = out
	cmd.Stderr = out

	err := cmd.Run()
	if probeCtx.Err() != nil && ctx.Err() == nil {
		// The probe's own deadline fired, not the run's: an observation that did not
		// happen. The caller must not record a result for it.
		return 0, out.tail(), true
	}
	if err != nil {
		if errors.Is(err, exec.ErrWaitDelay) {
			// The command finished; a background child held the pipe and WaitDelay cut
			// it off. The observation is the exit code, which was zero.
			return 0, out.tail(), false
		}
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), out.tail(), false
		}
		// The command could not be started at all (no /bin/sh, a permission problem):
		// a failed observation with the reason in the tail.
		return -1, err.Error(), false
	}
	return 0, out.tail(), false
}

// cappedBuffer keeps the first max bytes of what it is written. First, not last: a
// probe's first lines usually name what it is doing, and a stream that never stops
// would otherwise spend the run's memory to keep the tail nobody reads.
//
// Both of the child's pipes write here, and os/exec writes to non-file writers from
// a goroutine per pipe, so the lock is required, not defensive.
type cappedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.max - c.buf.Len(); room > 0 {
		if room > len(p) {
			room = len(p)
		}
		c.buf.Write(p[:room])
	}
	// Always report the full length: a short write would make the child see an error
	// on its own stdout and change the probe's behaviour.
	return len(p), nil
}

// tail is the stored form: at most watchProbeTail bytes, cut back to a rune boundary
// so the result is valid UTF-8 for the JSON payload it ends up in.
func (c *cappedBuffer) tail() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.buf.Bytes()
	if len(b) > watchProbeTail {
		b = b[:watchProbeTail]
	}
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}
