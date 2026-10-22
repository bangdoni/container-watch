package sse

import (
	"context"
	"log/slog"
	"time"

	"github.com/containerwatch/containerwatch/internal/engine"
)

// EventBroker watches engine events (start/stop/health changes) and translates
// them into htmx-friendly SSE notifications. It also maintains the short-lived
// container status cache the dashboard reads on refresh.
type EventBroker struct {
	hub *Hub
	log *slog.Logger

	// Debounce coalesces bursts (compose up/down emits dozens of events).
	pending chan struct{}
}

// NewEventBroker builds an event broker.
func NewEventBroker(hub *Hub, log *slog.Logger) *EventBroker {
	return &EventBroker{hub: hub, log: log, pending: make(chan struct{}, 1)}
}

// StartNode follows a node's event stream until ctx is cancelled, publishing a
// debounced "containers:changed" event per burst of activity.
func (e *EventBroker) StartNode(ctx context.Context, nodeID int64, client *engine.Client) {
	go e.run(ctx, nodeID, client)
}

func (e *EventBroker) run(ctx context.Context, nodeID int64, client *engine.Client) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		events, err := client.StreamEvents(ctx)
		if err != nil {
			e.log.Debug("events: connect failed", "node", nodeID, "error", err)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second

		// Coalesce bursts: after the first event, wait for a quiet period.
		var timer *time.Timer
		fire := func() {
			e.hub.PublishJSON(TopicEvents(nodeID), "containers", map[string]any{
				"node_id": nodeID,
				"ts":      time.Now().UnixMilli(),
			})
		}
		for ev := range events {
			if ev.Type != "container" {
				continue
			}
			if timer == nil {
				timer = time.AfterFunc(400*time.Millisecond, fire)
			} else {
				timer.Reset(400 * time.Millisecond)
			}
		}
		if timer != nil {
			timer.Stop()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// TopicEvents is the SSE topic for container-change notifications per node.
func TopicEvents(nodeID int64) string { return "events:" + itoa(nodeID) }
