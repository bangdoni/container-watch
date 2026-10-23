package sse_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/bangdoni/container-watch/internal/sse"
)

// TestHubPublishReachesSubscriber checks the basic fan-out.
func TestHubPublishReachesSubscriber(t *testing.T) {
	hub := sse.NewHub()
	ch, cancel := hub.Subscribe("topic")
	defer cancel()

	hub.Publish("topic", "hello", []byte("payload"))

	select {
	case ev := <-ch:
		if ev.Name != "hello" {
			t.Errorf("event name = %q, want hello", ev.Name)
		}
		if string(ev.Data) != "payload" {
			t.Errorf("event data = %q, want payload", ev.Data)
		}
	case <-time.After(time.Second):
		t.Fatal("no event was delivered")
	}
}

// TestHubIsolatesTopics checks that a subscriber only gets events for the
// topic it subscribed to.
func TestHubIsolatesTopics(t *testing.T) {
	hub := sse.NewHub()
	ch, cancel := hub.Subscribe("alpha")
	defer cancel()

	hub.Publish("beta", "x", []byte("1"))

	select {
	case ev := <-ch:
		t.Fatalf("received an event for another topic: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestHubUnsubscribeStopsDelivery checks that the cancel function returned
// by Subscribe actually detaches the subscriber.
func TestHubUnsubscribeStopsDelivery(t *testing.T) {
	hub := sse.NewHub()
	ch, cancel := hub.Subscribe("topic")
	cancel()

	hub.Publish("topic", "hello", []byte("payload"))

	select {
	case ev := <-ch:
		t.Fatalf("received an event after unsubscribing: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestHubDropsForSlowSubscriber checks the "slow subscribers are dropped,
// not blocked" contract: a subscriber that never reads must not block the
// publisher, and the events beyond its buffer are discarded rather than
// queued forever.
func TestHubDropsForSlowSubscriber(t *testing.T) {
	hub := sse.NewHub()
	_, cancel := hub.Subscribe("topic")
	defer cancel()

	// The subscriber buffer is 8 deep. Publishing far more than that
	// must not block.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			hub.Publish("topic", "spam", []byte("x"))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a slow subscriber")
	}
}

// TestHubPublishJSON checks the JSON convenience wrapper.
func TestHubPublishJSON(t *testing.T) {
	hub := sse.NewHub()
	ch, cancel := hub.Subscribe("topic")
	defer cancel()

	hub.PublishJSON("topic", "update", map[string]any{"cpu": 12.5})

	select {
	case ev := <-ch:
		var got map[string]any
		if err := json.Unmarshal(ev.Data, &got); err != nil {
			t.Fatalf("event data is not JSON: %v", err)
		}
		if got["cpu"] != 12.5 {
			t.Errorf("cpu = %v, want 12.5", got["cpu"])
		}
	case <-time.After(time.Second):
		t.Fatal("no event was delivered")
	}
}

// TestHubPublishJSONIgnoresUnmarshalable checks that a value which cannot
// be marshalled is dropped instead of panicking.
func TestHubPublishJSONIgnoresUnmarshalable(t *testing.T) {
	hub := sse.NewHub()
	ch, cancel := hub.Subscribe("topic")
	defer cancel()

	// A channel cannot be marshalled to JSON.
	hub.PublishJSON("topic", "bad", make(chan int))

	select {
	case ev := <-ch:
		t.Fatalf("an unmarshalable value produced an event: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestTopicMetrics checks the topic naming helpers, which the browser
// listens on.
func TestTopicMetrics(t *testing.T) {
	if got := sse.TopicMetrics(7); got != "metrics:7" {
		t.Errorf("TopicMetrics(7) = %q, want metrics:7", got)
	}
	if got := sse.TopicEvents(3); got != "events:3" {
		t.Errorf("TopicEvents(3) = %q, want events:3", got)
	}
}

// TestHubConcurrentSubscribePublish exercises the hub from many goroutines
// at once. Run with -race it would catch a data race on the topic map.
func TestHubConcurrentSubscribePublish(t *testing.T) {
	hub := sse.NewHub()
	done := make(chan struct{})

	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				ch, cancel := hub.Subscribe("topic")
				hub.Publish("topic", "e", []byte("d"))
				cancel()
				_ = ch
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}
