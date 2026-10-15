package state

import (
	"fmt"
	"strings"
	"time"
)

// Keep bounded timings without logging successful writes or sample values.
// Capture a failure before rollback, which may take time of its own.
type historyWriteTiming struct {
	started time.Time
	step    time.Time
	phase   string
	phases  [12]struct {
		name    string
		elapsed time.Duration
	}
	n int
}

func newHistoryWriteTiming(phase string) historyWriteTiming {
	now := time.Now()
	return historyWriteTiming{started: now, step: now, phase: phase}
}

func (t *historyWriteTiming) next(phase string) {
	now := time.Now()
	t.add(t.phase, now.Sub(t.step))
	t.phase, t.step = phase, now
}

func (t *historyWriteTiming) add(phase string, elapsed time.Duration) {
	for i := 0; i < t.n; i++ {
		if t.phases[i].name == phase {
			t.phases[i].elapsed += elapsed
			return
		}
	}
	if t.n < len(t.phases) {
		t.phases[t.n].name, t.phases[t.n].elapsed = phase, elapsed
		t.n++
	}
}

func (t *historyWriteTiming) failure(err error) error {
	if err == nil {
		return nil
	}
	now := time.Now()
	t.add(t.phase, now.Sub(t.step))
	var details strings.Builder
	for i := 0; i < t.n; i++ {
		fmt.Fprintf(&details, " %s_ms=%d", t.phases[i].name, t.phases[i].elapsed.Milliseconds())
	}
	return fmt.Errorf("%w [elapsed_ms=%d phase=%s step_ms=%d%s]", err, now.Sub(t.started).Milliseconds(), t.phase, now.Sub(t.step).Milliseconds(), details.String())
}
