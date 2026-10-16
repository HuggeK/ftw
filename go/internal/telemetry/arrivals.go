package telemetry

// Arrived tells every reader of Arrivals that a driver finished a poll, so
// a reader that streams live values can send them as they land instead of
// on a clock of its own. The driver registry calls it once per poll, after
// the poll's readings are in the store: a poll's readings belong together,
// and the house load is derived from all of them at once, so a reader that
// woke on each emit could send a load that never existed.
func (s *Store) Arrived() {
	s.arrivalsMu.Lock()
	defer s.arrivalsMu.Unlock()
	for ch := range s.arrivals {
		select {
		case ch <- struct{}{}:
		default:
			// One is waiting already; this poll merges into it.
		}
	}
}

// Arrivals returns a channel that receives after each finished poll, and a
// func that stops it. Polls that finish while a signal waits merge into
// that signal, so a slow reader never holds up a driver.
func (s *Store) Arrivals() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.arrivalsMu.Lock()
	if s.arrivals == nil {
		s.arrivals = map[chan struct{}]struct{}{}
	}
	s.arrivals[ch] = struct{}{}
	s.arrivalsMu.Unlock()
	return ch, func() {
		s.arrivalsMu.Lock()
		delete(s.arrivals, ch)
		s.arrivalsMu.Unlock()
	}
}
