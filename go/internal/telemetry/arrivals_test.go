package telemetry

import "testing"

// Every reader hears a finished poll; polls that finish while one waits
// merge into it; a stopped reader hears nothing more.
func TestArrivalsReachEveryReaderAndMerge(t *testing.T) {
	s := NewStore()
	a, stopA := s.Arrivals()
	b, stopB := s.Arrivals()
	defer stopB()

	s.Arrived()
	s.Arrived()
	for name, ch := range map[string]<-chan struct{}{"a": a, "b": b} {
		select {
		case <-ch:
		default:
			t.Fatalf("reader %s heard no arrival", name)
		}
		select {
		case <-ch:
			t.Fatalf("reader %s heard two polls as two", name)
		default:
		}
	}

	stopA()
	s.Arrived()
	select {
	case <-a:
		t.Fatal("a stopped reader heard an arrival")
	default:
	}
	<-b
}
