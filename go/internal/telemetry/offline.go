package telemetry

import "sort"

// OfflineDrivers names the drivers with a reading of kind whose telemetry is
// offline or has no health yet. Only names leave the store, never a cached
// value, so a stale reading cannot be mistaken for a live one.
func (s *Store) OfflineDrivers(kind DerType) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for _, r := range s.readings {
		if r.DerType != kind {
			continue
		}
		if h := s.health[r.Driver]; h != nil && h.Status != StatusOffline {
			continue
		}
		out = append(out, r.Driver)
	}
	sort.Strings(out)
	return out
}
