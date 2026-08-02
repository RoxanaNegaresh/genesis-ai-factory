// Package bus implements in-process event fan-out with bounded buffers.
//
// The central design decision: a slow consumer must never be able to stall the
// factory. Publishing is therefore non-blocking. When a subscriber's buffer is
// full the oldest events are dropped and collapsed into a gap marker; the
// client refetches the missing range over REST using the durable event log.
// Losing delivery is acceptable because the log is the source of truth; losing
// liveness is not.
package bus

import (
	"sync"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/port"
)

// DefaultBuffer is the per-subscriber queue depth.
const DefaultBuffer = 256

// Bus is a topic-based publish/subscribe hub.
type Bus struct {
	mu     sync.RWMutex
	subs   map[*subscription]struct{}
	closed bool
}

// New constructs an empty bus.
func New() *Bus {
	return &Bus{subs: map[*subscription]struct{}{}}
}

var _ port.Bus = (*Bus)(nil)

// Publish delivers an event to every subscriber of its topic. It never blocks.
func (b *Bus) Publish(e *domain.Event) {
	if e == nil {
		return
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return
	}
	for s := range b.subs {
		s.deliver(e)
	}
}

// Subscribe registers a new subscriber for the given topics.
func (b *Bus) Subscribe(buffer int, topics ...string) port.Subscription {
	if buffer <= 0 {
		buffer = DefaultBuffer
	}
	s := &subscription{
		bus:    b,
		events: make(chan *domain.Event, buffer),
		gaps:   make(chan port.Gap, 16),
		topics: map[string]struct{}{},
		buffer: buffer,
	}
	for _, t := range topics {
		s.topics[t] = struct{}{}
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		close(s.events)
		close(s.gaps)
		s.closed = true
		return s
	}
	b.subs[s] = struct{}{}
	return s
}

// SubscriberCount reports live subscribers, used by the metrics endpoint.
func (b *Bus) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}

// Close terminates every subscription.
func (b *Bus) Close() {
	b.mu.Lock()
	subs := make([]*subscription, 0, len(b.subs))
	for s := range b.subs {
		subs = append(subs, s)
	}
	b.subs = map[*subscription]struct{}{}
	b.closed = true
	b.mu.Unlock()

	for _, s := range subs {
		s.shutdown()
	}
}

func (b *Bus) remove(s *subscription) {
	b.mu.Lock()
	delete(b.subs, s)
	b.mu.Unlock()
}

// subscription is one client's view of the stream.
type subscription struct {
	bus    *Bus
	events chan *domain.Event
	gaps   chan port.Gap
	buffer int

	mu     sync.RWMutex
	topics map[string]struct{}
	closed bool

	// Pending gap state, coalesced so a long stall produces one marker rather
	// than thousands.
	gapMu    sync.Mutex
	gapFrom  int64
	gapTo    int64
	gapTopic string
	gapOpen  bool
}

func (s *subscription) Events() <-chan *domain.Event { return s.events }
func (s *subscription) Gaps() <-chan port.Gap        { return s.gaps }

// Subscribe adds topics to a live subscription.
func (s *subscription) Subscribe(topics ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range topics {
		s.topics[t] = struct{}{}
	}
}

// Unsubscribe removes topics from a live subscription.
func (s *subscription) Unsubscribe(topics ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range topics {
		delete(s.topics, t)
	}
}

// Close detaches the subscriber and releases its channels.
func (s *subscription) Close() {
	s.bus.remove(s)
	s.shutdown()
}

func (s *subscription) shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.events)
	close(s.gaps)
}

func (s *subscription) interested(topic string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return false
	}
	_, ok := s.topics[topic]
	return ok
}

// deliver enqueues an event, degrading to a gap marker under backpressure.
func (s *subscription) deliver(e *domain.Event) {
	if !s.interested(e.Topic) {
		return
	}

	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return
	}

	defer func() {
		// A concurrent Close may close the channel between the check above and
		// the send below. Recovering here is cheaper and less deadlock-prone
		// than holding the subscription lock across a channel send.
		_ = recover()
	}()

	select {
	case s.events <- e:
		s.flushGap()
	default:
		// Buffer full: drop the oldest event to make room for the newest, so
		// the client always sees current state rather than stale history.
		select {
		case dropped := <-s.events:
			s.noteGap(dropped)
			select {
			case s.events <- e:
			default:
				s.noteGap(e)
			}
		default:
			s.noteGap(e)
		}
	}
}

// noteGap records that an event was dropped, extending any open marker.
func (s *subscription) noteGap(e *domain.Event) {
	s.gapMu.Lock()
	defer s.gapMu.Unlock()
	if !s.gapOpen {
		s.gapOpen = true
		s.gapTopic = e.Topic
		s.gapFrom = e.Seq
		s.gapTo = e.Seq
		return
	}
	if e.Seq < s.gapFrom {
		s.gapFrom = e.Seq
	}
	if e.Seq > s.gapTo {
		s.gapTo = e.Seq
	}
}

// flushGap emits the coalesced marker once the consumer has caught up.
func (s *subscription) flushGap() {
	s.gapMu.Lock()
	if !s.gapOpen {
		s.gapMu.Unlock()
		return
	}
	gap := port.Gap{Topic: s.gapTopic, From: s.gapFrom, To: s.gapTo}
	s.gapOpen = false
	s.gapMu.Unlock()

	defer func() { _ = recover() }()
	select {
	case s.gaps <- gap:
	default:
		// Even the gap channel is full: the client is comprehensively behind
		// and will resync from its cursor on the next reconnect.
	}
}
