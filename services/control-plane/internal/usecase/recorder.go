package usecase

import (
	"context"
	"log/slog"

	"github.com/genesis-ai-factory/control-plane/internal/domain"
	"github.com/genesis-ai-factory/control-plane/internal/port"
)

// Recorder persists an event and publishes it in a single call.
//
// Every state change in the factory must be both durable and observable.
// Exposing the repository and the bus separately would make "persisted but
// never announced" a one-line mistake; this type makes it impossible.
type Recorder struct {
	events port.EventRepository
	bus    port.Publisher
	log    *slog.Logger
}

// NewRecorder constructs a recorder.
func NewRecorder(events port.EventRepository, bus port.Publisher, log *slog.Logger) *Recorder {
	if log == nil {
		log = slog.Default()
	}
	return &Recorder{events: events, bus: bus, log: log}
}

var _ port.Recorder = (*Recorder)(nil)

// Record appends the event to the durable log, then broadcasts it. Order
// matters: the sequence number is assigned by the store, and clients use it as
// a resume cursor, so an event must never be broadcast before it is durable.
func (r *Recorder) Record(ctx context.Context, e *domain.Event) error {
	if e == nil {
		return nil
	}
	if err := r.events.Append(ctx, e); err != nil {
		r.log.Error("failed to persist event",
			"type", e.Type, "topic", e.Topic, "run_id", e.RunID.String(), "error", err)
		return err
	}
	if r.bus != nil {
		r.bus.Publish(e)
	}
	return nil
}

// Emit records an event and swallows the error after logging it.
//
// Used on paths where losing an event must not abort the operation that
// produced it: failing a build because a log line could not be written would
// trade a cosmetic problem for a real one.
func (r *Recorder) Emit(ctx context.Context, e *domain.Event) {
	if err := r.Record(ctx, e); err != nil {
		r.log.Warn("event dropped", "type", e.Type, "error", err)
	}
}
