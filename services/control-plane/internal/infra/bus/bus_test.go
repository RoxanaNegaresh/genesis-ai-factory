package bus_test

import (
	"sync"
	"testing"
	"time"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/infra/bus"
)

func event(topic string, seq int64) *domain.Event {
	e := domain.NewEvent(topic, domain.EventLog, domain.LevelInfo, "msg")
	e.Seq = seq
	return e
}

func TestPublishReachesOnlySubscribedTopics(t *testing.T) {
	b := bus.New()
	defer b.Close()

	runA := domain.RunTopic(domain.NewID())
	runB := domain.RunTopic(domain.NewID())

	sub := b.Subscribe(8, runA)
	defer sub.Close()

	b.Publish(event(runB, 1))
	b.Publish(event(runA, 2))

	select {
	case got := <-sub.Events():
		if got.Topic != runA || got.Seq != 2 {
			t.Fatalf("received wrong event: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("subscribed event was not delivered")
	}

	select {
	case got := <-sub.Events():
		t.Fatalf("received event for unsubscribed topic: %+v", got)
	default:
	}
}

func TestDynamicSubscribeUnsubscribe(t *testing.T) {
	b := bus.New()
	defer b.Close()

	topic := domain.RunTopic(domain.NewID())
	sub := b.Subscribe(8)
	defer sub.Close()

	b.Publish(event(topic, 1))
	select {
	case <-sub.Events():
		t.Fatal("event delivered before subscribing to the topic")
	default:
	}

	sub.Subscribe(topic)
	b.Publish(event(topic, 2))
	select {
	case got := <-sub.Events():
		if got.Seq != 2 {
			t.Fatalf("unexpected event %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("event not delivered after subscribing")
	}

	sub.Unsubscribe(topic)
	b.Publish(event(topic, 3))
	select {
	case got := <-sub.Events():
		t.Fatalf("event delivered after unsubscribing: %+v", got)
	default:
	}
}

// The critical property: a subscriber that never reads must not block the
// publisher. This is what keeps a stalled UI from freezing the factory.
func TestSlowSubscriberDoesNotBlockPublisher(t *testing.T) {
	b := bus.New()
	defer b.Close()

	topic := domain.RunTopic(domain.NewID())
	slow := b.Subscribe(4, topic)
	defer slow.Close()

	done := make(chan struct{})
	go func() {
		for i := int64(1); i <= 10_000; i++ {
			b.Publish(event(topic, i))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("publisher blocked on a slow subscriber")
	}
}

func TestBackpressureEmitsGapAndKeepsNewestEvents(t *testing.T) {
	b := bus.New()
	defer b.Close()

	topic := domain.RunTopic(domain.NewID())
	sub := b.Subscribe(4, topic)
	defer sub.Close()

	for i := int64(1); i <= 20; i++ {
		b.Publish(event(topic, i))
	}

	// Drain the buffer; the retained events must be the most recent ones.
	var got []int64
	for {
		select {
		case e := <-sub.Events():
			got = append(got, e.Seq)
			continue
		default:
		}
		break
	}
	if len(got) == 0 {
		t.Fatal("no events retained")
	}
	if last := got[len(got)-1]; last != 20 {
		t.Fatalf("newest event was dropped: last seq %d", last)
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("events out of order: %v", got)
		}
	}

	// A gap marker must be produced for the dropped range. The marker is
	// flushed on the next successful delivery.
	b.Publish(event(topic, 21))
	select {
	case gap := <-sub.Gaps():
		if gap.Topic != topic {
			t.Fatalf("gap on wrong topic: %+v", gap)
		}
		if gap.From < 1 || gap.To > 21 || gap.From > gap.To {
			t.Fatalf("nonsensical gap range: %+v", gap)
		}
	case <-time.After(time.Second):
		t.Fatal("no gap marker emitted after dropping events")
	}
}

func TestConcurrentPublishAndSubscribe(t *testing.T) {
	b := bus.New()
	defer b.Close()

	topic := domain.RunTopic(domain.NewID())
	const publishers, subscribers, perPublisher = 4, 8, 200

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < subscribers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sub := b.Subscribe(16, topic)
			defer sub.Close()
			for {
				select {
				case <-sub.Events():
				case <-sub.Gaps():
				case <-stop:
					return
				}
			}
		}()
	}

	var pub sync.WaitGroup
	for p := 0; p < publishers; p++ {
		pub.Add(1)
		go func() {
			defer pub.Done()
			for i := int64(1); i <= perPublisher; i++ {
				b.Publish(event(topic, i))
			}
		}()
	}

	done := make(chan struct{})
	go func() { pub.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("concurrent publishing deadlocked")
	}

	close(stop)
	wg.Wait()
}

func TestCloseIsIdempotentAndSafeUnderPublish(t *testing.T) {
	b := bus.New()
	topic := domain.RunTopic(domain.NewID())
	sub := b.Subscribe(4, topic)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := int64(0); i < 2000; i++ {
			b.Publish(event(topic, i))
		}
	}()

	time.Sleep(2 * time.Millisecond)
	sub.Close()
	sub.Close() // must not panic on double close
	wg.Wait()

	b.Close()
	b.Close()

	// Publishing after close must be a no-op, not a panic.
	b.Publish(event(topic, 9999))

	if n := b.SubscriberCount(); n != 0 {
		t.Fatalf("expected 0 subscribers after close, got %d", n)
	}
}

func TestSubscriberCount(t *testing.T) {
	b := bus.New()
	defer b.Close()

	if b.SubscriberCount() != 0 {
		t.Fatal("new bus should have no subscribers")
	}
	a := b.Subscribe(4, "system")
	c := b.Subscribe(4, "system")
	if b.SubscriberCount() != 2 {
		t.Fatalf("expected 2 subscribers, got %d", b.SubscriberCount())
	}
	a.Close()
	if b.SubscriberCount() != 1 {
		t.Fatalf("expected 1 subscriber after close, got %d", b.SubscriberCount())
	}
	c.Close()
}
