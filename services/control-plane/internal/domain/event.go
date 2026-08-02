package domain

import (
	"fmt"
	"time"
)

// EventType is the closed set of things the factory can announce. The desktop
// app and CLI switch on these, so they are part of the public contract.
type EventType string

const (
	EventRunCreated      EventType = "run.created"
	EventRunStarted      EventType = "run.started"
	EventRunCompleted    EventType = "run.completed"
	EventRunFailed       EventType = "run.failed"
	EventRunCanceled     EventType = "run.canceled"
	EventPhaseStarted    EventType = "phase.started"
	EventPhaseCompleted  EventType = "phase.completed"
	EventPhaseFailed     EventType = "phase.failed"
	EventAgentAssigned   EventType = "agent.assigned"
	EventAgentThinking   EventType = "agent.thinking"
	EventAgentCompleted  EventType = "agent.completed"
	EventAgentFailed     EventType = "agent.failed"
	EventToolInvoked     EventType = "tool.invoked"
	EventFileWritten     EventType = "file.written"
	EventCommandExecuted EventType = "command.executed"
	EventTestCompleted   EventType = "test.completed"
	EventErrorDetected   EventType = "error.detected"
	EventHealAttempted   EventType = "heal.attempted"
	EventArtifactCreated EventType = "artifact.created"
	EventProjectUpdated  EventType = "project.updated"
	EventLog             EventType = "log"
)

// Level is the severity of an event, used for UI filtering and log routing.
type Level string

const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// Event is an immutable fact appended to the run log. The UI is a pure
// projection of this stream: if a state change is not announced here, no client
// can observe it.
type Event struct {
	Seq       int64     `json:"seq"`
	ID        ID        `json:"id"`
	RunID     ID        `json:"run_id,omitempty"`
	ProjectID ID        `json:"project_id,omitempty"`
	Topic     string    `json:"topic"`
	Type      EventType `json:"type"`
	AgentRole AgentRole `json:"agent_role,omitempty"`
	Level     Level     `json:"level"`
	Message   string    `json:"message"`
	Payload   Settings  `json:"payload,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Topic naming is centralised so publishers and subscribers cannot disagree
// about the string format.
func RunTopic(runID ID) string   { return "run:" + runID.String() }
func ProjectTopic(pID ID) string { return "project:" + pID.String() }

const SystemTopic = "system"

// ValidTopic guards subscription requests from clients.
func ValidTopic(topic string) bool {
	if topic == SystemTopic {
		return true
	}
	for _, prefix := range []string{"run:", "project:"} {
		if len(topic) > len(prefix) && topic[:len(prefix)] == prefix {
			if _, err := ParseID(topic[len(prefix):]); err == nil {
				return true
			}
		}
	}
	return false
}

// NewEvent constructs an event with a generated id and timestamp. Seq is
// assigned by the store on append, because only the store can guarantee a
// gapless global ordering.
func NewEvent(topic string, typ EventType, level Level, message string) *Event {
	return &Event{
		ID:        NewID(),
		Topic:     topic,
		Type:      typ,
		Level:     level,
		Message:   message,
		Payload:   Settings{},
		CreatedAt: time.Now().UTC(),
	}
}

// For attaches run/project scope to an event and derives its topic.
func (e *Event) For(runID, projectID ID) *Event {
	e.RunID = runID
	e.ProjectID = projectID
	if e.Topic == "" && !runID.IsZero() {
		e.Topic = RunTopic(runID)
	}
	return e
}

// By attributes the event to an agent.
func (e *Event) By(role AgentRole) *Event {
	e.AgentRole = role
	return e
}

// With adds a payload field.
func (e *Event) With(key string, value any) *Event {
	if e.Payload == nil {
		e.Payload = Settings{}
	}
	e.Payload[key] = value
	return e
}

// String renders a one-line human summary, used by the CLI log tail.
func (e *Event) String() string {
	if e.AgentRole != "" {
		return fmt.Sprintf("[%s] %s: %s", e.Type, e.AgentRole, e.Message)
	}
	return fmt.Sprintf("[%s] %s", e.Type, e.Message)
}
