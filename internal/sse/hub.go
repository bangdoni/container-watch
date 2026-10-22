// Package sse implements a tiny fan-out broker for Server-Sent Events.
package sse

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Event is one server-sent event.
type Event struct {
	Name string
	Data []byte
}

// Hub fan-outs events to per-topic subscribers. Slow subscribers are dropped
// rather than blocking the publisher, which keeps one stalled browser tab from
// stalling the monitor.
type Hub struct {
	mu     sync.RWMutex
	topics map[string]map[*subscriber]struct{}
}

type subscriber struct {
	ch chan Event
}

// NewHub creates an empty broker.
func NewHub() *Hub {
	return &Hub{topics: make(map[string]map[*subscriber]struct{})}
}

// Subscribe registers a listener for topic. The returned cancel function must
// be called when the client disconnects.
func (h *Hub) Subscribe(topic string) (<-chan Event, func()) {
	s := &subscriber{ch: make(chan Event, 8)}
	h.mu.Lock()
	if h.topics[topic] == nil {
		h.topics[topic] = make(map[*subscriber]struct{})
	}
	h.topics[topic][s] = struct{}{}
	h.mu.Unlock()

	return s.ch, func() {
		h.mu.Lock()
		if subs, ok := h.topics[topic]; ok {
			delete(subs, s)
			if len(subs) == 0 {
				delete(h.topics, topic)
			}
		}
		h.mu.Unlock()
	}
}

// Publish sends an event to every subscriber of topic without blocking.
func (h *Hub) Publish(topic, name string, data []byte) {
	ev := Event{Name: name, Data: data}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for s := range h.topics[topic] {
		select {
		case s.ch <- ev:
		default:
			// Subscriber is slow: drop this event, the next snapshot will
			// bring it up to date.
		}
	}
}

// PublishJSON marshals v and publishes it.
func (h *Hub) PublishJSON(topic, name string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	h.Publish(topic, name, b)
}

// Write sends SSE headers. Call it once per request before streaming.
func WriteHeaders(w http.ResponseWriter) http.Flusher {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	return flusher
}

// Send writes one event frame.
func Send(w http.ResponseWriter, ev Event) error {
	if ev.Name != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", ev.Name); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", ev.Data); err != nil {
		return err
	}
	return nil
}

// Comment writes an SSE comment (used as a keep-alive heartbeat).
func Comment(w http.ResponseWriter, text string) error {
	_, err := fmt.Fprintf(w, ": %s\n\n", text)
	return err
}

// KeepAlive starts a heartbeat goroutine that stops when done is closed. It is
// used to keep proxies from closing idle SSE connections.
func KeepAlive(w http.ResponseWriter, flusher http.Flusher, done <-chan struct{}, every time.Duration) {
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if err := Comment(w, "keep-alive"); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}()
}
