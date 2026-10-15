package drivers

import (
	"context"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestNextPollIntervalHonoursTheReturnUnlessSetPollIntervalWasCalled(t *testing.T) {
	cases := []struct {
		name       string
		standingMS int32 // 0: the driver never called host.set_poll_interval
		returned   time.Duration
		want       time.Duration
	}{
		{"nothing returned uses the default", 0, 0, 5 * time.Second},
		{"a returned wait sets the next poll", 0, time.Second, time.Second},
		{"a fast return is held to one second", 0, 200 * time.Millisecond, time.Second},
		{"a long return is held to thirty seconds", 0, time.Minute, 30 * time.Second},
		{"set_poll_interval wins over a shorter return", 2000, time.Second, 2 * time.Second},
		{"set_poll_interval wins over a longer return", 2000, time.Minute, 2 * time.Second},
		{"set_poll_interval is not bounded", 10, 0, 10 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := NewHostEnv("probe", nil)
			if tc.standingMS > 0 {
				env.setPollInterval(tc.standingMS)
			}
			if got := env.nextPollInterval(tc.returned); got != tc.want {
				t.Fatalf("next wait = %v, want %v", got, tc.want)
			}
			if got := env.ScheduledPollInterval(); got != tc.want {
				t.Fatalf("scheduled wait = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReturnedPollIntervalCapsNumbersBeyondDuration(t *testing.T) {
	for _, ms := range []float64{1e16, 1e300} {
		if got := returnedPollInterval(ms); got != 30*time.Second {
			t.Fatalf("returnedPollInterval(%g) = %v, want 30s", ms, got)
		}
	}
}

// returnedRuntime returns a fixed interval from every poll and reports when
// each poll ran.
type returnedRuntime struct {
	env      *HostEnv
	returned time.Duration
	polls    chan time.Time
}

func (r *returnedRuntime) Init(context.Context, []byte) error { return nil }
func (r *returnedRuntime) Poll(context.Context) (time.Duration, error) {
	r.polls <- time.Now()
	return r.returned, nil
}
func (r *returnedRuntime) Command(context.Context, []byte) error { return nil }
func (r *returnedRuntime) DefaultMode(context.Context) error     { return nil }
func (r *returnedRuntime) Cleanup(context.Context) error         { return nil }
func (r *returnedRuntime) Discard()                              {}
func (r *returnedRuntime) Env() *HostEnv                         { return r.env }

// A driver that only returns its interval, as sungrow.lua does, is polled at
// that interval rather than the 5 s default.
func TestRunLoopWaitsTheIntervalThePollReturned(t *testing.T) {
	tel := telemetry.NewStore()
	r := NewRegistry(tel)
	env := NewHostEnv("d1", tel)
	// Bring the first poll forward without marking the interval as set by
	// the driver; the default would otherwise delay it five seconds.
	env.PollIntervalMS = 10
	rt := &returnedRuntime{env: env, returned: time.Second, polls: make(chan time.Time, 8)}
	rd := &runningDriver{
		driver:    rt,
		env:       env,
		cfg:       config.Driver{Name: "d1"},
		cmdCh:     make(chan driverCmd, 1),
		defaultCh: make(chan driverCmd, 1),
		stop:      make(chan bool, 1),
		done:      make(chan struct{}),
	}
	r.rec["d1"] = rd
	go r.runLoop(rd)
	t.Cleanup(func() { r.remove("d1", true) })

	next := func() time.Time {
		t.Helper()
		select {
		case at := <-rt.polls:
			return at
		case <-time.After(3 * time.Second):
			t.Fatal("no poll within 3 s")
			return time.Time{}
		}
	}
	first := next()
	second := next()
	if gap := second.Sub(first); gap < 950*time.Millisecond || gap > 1500*time.Millisecond {
		t.Fatalf("second poll %v after the first, want the returned second", gap)
	}
	if got, _ := r.PollInterval("d1"); got != time.Second {
		t.Fatalf("PollInterval = %v, want the returned 1s", got)
	}
}

// A driver that set its own cadence keeps it, whatever driver_poll returns.
func TestRunLoopKeepsSetPollIntervalOverTheReturn(t *testing.T) {
	path := writeTestDriver(t, `
local polls = 0
function driver_init(config) host.set_poll_interval(10) end
function driver_poll()
  polls = polls + 1
  host.emit_metric("polls", polls)
  return 60000
end
function driver_command() end
function driver_default_mode() end
`)
	tel := telemetry.NewStore()
	reg := NewRegistry(tel)
	if err := reg.Add(context.Background(), config.Driver{Name: "standing", Lua: path}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.remove("standing", true) })

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if value, _, _ := tel.LatestMetric("standing", "polls"); value >= 3 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	value, _, _ := tel.LatestMetric("standing", "polls")
	t.Fatalf("polled %v times in 2 s at a 10 ms set_poll_interval", value)
}
