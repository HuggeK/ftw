package notifications

import "context"

// NotifyCloudGrant tells every owner that FTW Cloud was turned on or off,
// with the catalogue's sentence for kind. The rule gates it like any other,
// but a box whose stored rules predate these kinds still sends it: a missing
// rule reads as the default, which is on.
func (s *Service) NotifyCloudGrant(kind string) {
	if s == nil || (kind != PushCloudGranted && kind != PushCloudRevoked) {
		return
	}
	s.mu.Lock()
	if s.cfg == nil || !s.cfg.Enabled {
		s.mu.Unlock()
		return
	}
	rule, ok := findRule(s.cfg.Events, kind)
	if !ok {
		rule, _ = findRule(DefaultRules(), kind)
	}
	prio := rule.Priority
	if prio == 0 {
		prio = s.cfg.DefaultPriority
	}
	s.mu.Unlock()
	if !rule.Enabled {
		return
	}

	title, body, err := RenderPush(kind, nil)
	if err != nil {
		s.bumpFailed()
		s.emitDispatched(kind, "", Message{Priority: prio}, "failed", err.Error())
		return
	}
	s.deliver(kind, "", Message{Kind: kind, Title: title, Body: body, Priority: prio, Tags: splitTags(rule.Tags)})
}

// CloudPublisher hands each catalogue-worded message to FTW Cloud's uplink,
// which signs it and sends it as an event for the owners' phones. It only
// queues: the uplink sends on its own goroutine, so a slow or absent service
// never holds up another transport. Template rules carry no kind and stay on
// the box's own transports.
type CloudPublisher struct {
	// Queue takes one message and reports whether FTW Cloud will carry it.
	// False means FTW Cloud is off or its grant leaves events out.
	Queue func(Message) bool
}

// Publish queues m, or answers ErrNothingToSend when FTW Cloud will not
// carry it.
func (p CloudPublisher) Publish(_ context.Context, m Message) error {
	if m.Kind == "" || p.Queue == nil || !p.Queue(m) {
		return ErrNothingToSend
	}
	return nil
}
