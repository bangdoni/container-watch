package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// LogLine is one line of container output.
type LogLine struct {
	// Stream is "stdout" or "stderr".
	Stream string
	Text   string
}

// LogOptions configures a log stream request.
type LogOptions struct {
	// Tail is the number of historical lines to send before following.
	Tail int
	// Since (unix seconds, 0 = all) restricts history.
	Since int64
	// Timestamps prefixes each line with its engine timestamp.
	Timestamps bool
	// Follow keeps the connection open for new output.
	Follow bool
}

// Logs streams container output, calling onLine for each complete line. It
// returns when the stream ends, ctx is cancelled, or onLine returns an error
// (used by SSE handlers to stop when the client disconnects).
//
// Lines longer than maxLineBytes are split so a runaway writer cannot exhaust
// memory.
func (c *Client) Logs(ctx context.Context, id string, opts LogOptions, onLine func(LogLine) error) error {
	if err := ValidateID(id); err != nil {
		return err
	}
	q := url.Values{}
	if opts.Tail > 0 {
		q.Set("tail", strconv.Itoa(opts.Tail))
	} else {
		q.Set("tail", "100")
	}
	q.Set("stdout", "1")
	q.Set("stderr", "1")
	if opts.Follow {
		q.Set("follow", "1")
	}
	if opts.Since > 0 {
		q.Set("since", strconv.FormatInt(opts.Since, 10))
	}
	if opts.Timestamps {
		q.Set("timestamps", "1")
	}

	req, err := c.newRequest(ctx, http.MethodGet, "/containers/"+id+"/logs", q, nil)
	if err != nil {
		return err
	}
	// Streaming responses must not be bounded by the client timeout.
	cli := *c.http
	cli.Timeout = 0
	resp, err := cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusBadRequest:
		body, _ := readLimited(resp.Body, 512)
		// Some engines reject a non-TTY container log request; surface it.
		return errors.New("engine: logs: " + strings.TrimSpace(string(body)))
	default:
		return errors.New("engine: logs " + id + ": " + resp.Status)
	}

	src := newDemuxReader(resp.Body)

	return scanLines(ctx, src, maxLogLineBytes, onLine)
}

const maxLogLineBytes = 64 << 10

// scanLines splits r into lines, preserving partial lines across reads.
func scanLines(ctx context.Context, r io.Reader, maxLen int, onLine func(LogLine) error) error {
	br := bufio.NewReaderSize(r, 32<<10)
	var buf strings.Builder
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk, err := br.ReadString('\n')
		if chunk != "" {
			buf.WriteString(chunk)
			if buf.Len() >= maxLen || strings.HasSuffix(chunk, "\n") {
				text := strings.TrimSuffix(buf.String(), "\n")
				text = strings.TrimSuffix(text, "\r")
				// The demuxer flattens stdout/stderr, so the stream label is
				// unknown here; stderr detection happens in Logs via framing.
				if e := onLine(LogLine{Stream: "stdout", Text: text}); e != nil {
					return e
				}
				buf.Reset()
			}
		}
		if err != nil {
			if buf.Len() > 0 {
				text := strings.TrimSuffix(buf.String(), "\r")
				if e := onLine(LogLine{Stream: "stdout", Text: text}); e != nil {
					return e
				}
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// SinceTime converts a time to the engine's `since` argument.
func SinceTime(t time.Time) int64 { return t.Unix() }

// ------------------------------------------------------------- events ----

// Event is a Docker engine event (start, die, health_status, ...).
type Event struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
	Time     int64 `json:"time"`
	TimeNano int64 `json:"timeNano"`
}

// ContainerName returns the name of the involved container, if any.
func (e *Event) ContainerName() string {
	if e.Actor.Attributes == nil {
		return ""
	}
	return strings.TrimPrefix(e.Actor.Attributes["name"], "/")
}

// StreamEvents follows the engine event stream for containers and health
// status changes. Events are delivered on the returned channel until ctx ends.
func (c *Client) StreamEvents(ctx context.Context) (<-chan Event, error) {
	q := url.Values{}
	q.Add("filters", `{"type":["container"],"event":["start","stop","die","kill","pause","unpause","restart","destroy","create","health_status","rename"]}`)
	req, err := c.newRequest(ctx, http.MethodGet, "/events", q, nil)
	if err != nil {
		return nil, err
	}
	cli := *c.http
	cli.Timeout = 0
	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, errors.New("engine: events: " + resp.Status)
	}
	out := make(chan Event, 16)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		dec := json.NewDecoder(resp.Body)
		for {
			var ev Event
			if err := dec.Decode(&ev); err != nil {
				return
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}
