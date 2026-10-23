package handlers

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/bangdoni/container-watch/internal/auth"
	"github.com/bangdoni/container-watch/internal/database"
	"github.com/bangdoni/container-watch/internal/engine"
	"github.com/bangdoni/container-watch/internal/sse"
)

// actionTimeout bounds a lifecycle call. Stop can legitimately take a while
// for containers with a long grace period.
const actionTimeout = 45 * time.Second

// userOf returns the authenticated user for template fragments.
func userOf(r *http.Request) any {
	if u := currentUser(r); u != nil {
		return u
	}
	return nil
}

// resolve finds the node and container id for a /containers/{id} route.
func (s *Server) resolve(r *http.Request) (*engine.Node, string, error) {
	id := r.PathValue("id")
	if err := engine.ValidateID(id); err != nil {
		return nil, "", err
	}
	n, ok := s.nodeParam(r)
	if !ok || n == nil {
		return nil, "", errors.New("no container engine is configured")
	}
	if n.Client == nil {
		return nil, "", errors.New("node " + n.Record.Name + " has no usable connection")
	}
	return n, id, nil
}

// ensureOnline probes a node once when its status is stale, so a page opened
// against a sleeping host does not immediately fail.
func (s *Server) ensureOnline(r *http.Request, n *engine.Node) *engine.Node {
	if n.Status.Online {
		return n
	}
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()
	_ = s.nodes.ProbeByID(ctx, n.Record.ID)
	if fresh, ok := s.nodes.Get(n.Record.ID); ok {
		return fresh
	}
	return n
}

// detailView is the payload for the container detail page and modal.
type detailView struct {
	viewData
	Row       row
	Inspect   *engine.Inspect
	InspectID string
	Ports     []struct {
		Container string
		Host      string
	}
	Env        []string
	Labels     map[string]string
	Command    string
	Started    time.Time
	Finished   time.Time
	Series     []sse.Sample
	HealthLogs []engine.HealthLog
	IsDetail   bool
	Error      string
}

// buildDetail assembles the inspect payload shared by the page and modal.
func (s *Server) buildDetail(r *http.Request, n *engine.Node, id string) detailView {
	v := detailView{IsDetail: true}

	cols, err := s.containersFor(r, n)
	if err == nil {
		rows := s.buildRows(n.Record.ID, cols)
		for _, rr := range rows {
			if rr.C.ID == id || rr.Short == id || rr.Name == id {
				v.Row = rr
				break
			}
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	insp, err := n.Client.InspectContainer(ctx, id)
	if err != nil {
		v.Error = err.Error()
		return v
	}
	v.Inspect = insp
	v.InspectID = insp.ID
	v.Ports = insp.SortedPorts()
	v.Env = append([]string{}, insp.Config.Env...)
	sort.Strings(v.Env)
	v.Labels = insp.Config.Labels
	v.Command = insp.CommandString()
	v.Started = insp.StartedAt()
	if insp.State.Health != nil {
		v.HealthLogs = insp.State.Health.Log
	}
	v.Series = s.metrics.Series(n.Record.ID, insp.ID)
	if v.Row.C != nil && s.metrics.Window() > 1 {
		v.Data = map[string]any{
			"chartKey": itoa64(n.Record.ID) + "/" + insp.ID,
			"series":   v.Series,
		}
	}
	if v.Row.C == nil {
		// Synthesise a row from the inspect payload when the list cache is
		// cold (e.g. a container created seconds ago).
		v.Row = row{
			Name:    strings.TrimPrefix(insp.Name, "/"),
			Short:   insp.ID[:minInt(12, len(insp.ID))],
			Image:   insp.Config.Image,
			State:   insp.State.Status,
			Command: v.Command,
			Accent:  stateAccent(insp.State.Status),
			Ports:   nil,
		}
	}
	return v
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// containerDetailPage renders the standalone container page.
func (s *Server) containerDetailPage(w http.ResponseWriter, r *http.Request) {
	vd, _, ok := s.page(w, r, "containers", "Container")
	if vd.Redirected {
		return
	}
	if !ok {
		http.Redirect(w, r, "/containers", http.StatusSeeOther)
		return
	}
	n, id, err := s.resolve(r)
	if err != nil {
		vd.Flash = err.Error()
		vd.FlashKind = "error"
		s.renderPage(w, r, http.StatusOK, "container_detail", "layout", detailView{viewData: vd, Error: err.Error()})
		return
	}
	n = s.ensureOnline(r, n)
	vd.Title = id
	v := s.buildDetail(r, n, id)
	v.viewData = vd
	if v.Row.Name != "" {
		vd.Title = v.Row.Name
		v.viewData = vd
	}
	status := http.StatusOK
	if v.Inspect == nil && v.Error != "" {
		status = http.StatusOK
	}
	s.renderPage(w, r, status, "container_detail", "layout", v)
}

// containerDetailPartial renders the inspect modal body.
func (s *Server) containerDetailPartial(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	n, id, err := s.resolve(r)
	if err != nil {
		v := detailView{IsDetail: false, Error: err.Error()}
		s.renderPartial(w, r, "container-detail", v)
		return
	}
	n = s.ensureOnline(r, n)
	v := s.buildDetail(r, n, id)
	s.renderPartial(w, r, "container-detail", v)
}

// containerMetricsPartial renders the live metrics modal.
func (s *Server) containerMetricsPartial(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	n, id, err := s.resolve(r)
	if err != nil {
		s.renderPartial(w, r, "container-metrics", detailView{Error: err.Error()})
		return
	}
	v := detailView{IsDetail: false}
	cols, cerr := s.containersFor(r, n)
	if cerr == nil {
		for _, rr := range s.buildRows(n.Record.ID, cols) {
			if rr.C.ID == id || rr.Short == id || rr.Name == id {
				v.Row = rr
				break
			}
		}
	}
	if v.Row.C == nil {
		// Deeper id (short hash) → resolve via inspect.
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		if insp, ierr := n.Client.InspectContainer(ctx, id); ierr == nil {
			v.InspectID = insp.ID
		} else {
			v.Error = ierr.Error()
		}
	} else {
		v.InspectID = v.Row.C.ID
	}
	if v.InspectID != "" {
		v.Series = s.metrics.Series(n.Record.ID, v.InspectID)
		if m, ok := s.metrics.Latest(n.Record.ID, v.InspectID); ok {
			v.Row.CPU = m.CPU
			v.Row.Mem = m.Mem
			v.Row.MemLimit = m.MemLimit
			v.Row.MemPct = m.MemPercent()
			v.Row.RxRate = m.RxRate
			v.Row.TxRate = m.TxRate
			v.Row.HasStats = true
		}
	}
	v.viewData = viewData{NodeID: n.Record.ID, Data: map[string]any{
		"node":   n.Record.ID,
		"nodeID": n.Record.ID,
		"series": v.Series,
	}}
	s.renderPartial(w, r, "container-metrics", v)
}

// containerLogsPartial renders the log viewer shell; the tail itself arrives
// over SSE from containerLogsStream.
func (s *Server) containerLogsPartial(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	n, id, err := s.resolve(r)
	if err != nil {
		s.renderPartial(w, r, "container-logs", detailView{Error: err.Error()})
		return
	}
	v := detailView{IsDetail: false}
	cols, cerr := s.containersFor(r, n)
	if cerr == nil {
		for _, rr := range s.buildRows(n.Record.ID, cols) {
			if rr.C.ID == id || rr.Short == id || rr.Name == id {
				v.Row = rr
				break
			}
		}
	}
	if v.Row.Name == "" {
		v.Row.Name = id
		v.Row.Short = id
	}
	v.Row.State = strings.ToLower(v.Row.State)
	v.InspectID = id
	v.viewData = viewData{
		NodeID: n.Record.ID,
		Data: map[string]any{
			"nodeID": n.Record.ID,
			"stream": "/containers/" + id + "/logs/stream?node=" + itoa64(n.Record.ID),
			"tail":   defaultStr(r.URL.Query().Get("tail"), "200"),
		},
	}
	s.renderPartial(w, r, "container-logs", v)
}

// containerLogsStream tails container logs over SSE.
func (s *Server) containerLogsStream(w http.ResponseWriter, r *http.Request) {
	n, id, err := s.resolve(r)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	tail := parseIntDefault(r.URL.Query().Get("tail"), 200)
	switch {
	case tail <= 0:
		tail = 50
	case tail > 5000:
		tail = 5000
	}
	since := atoi64(r.URL.Query().Get("since"), 0)

	flusher := sse.WriteHeaders(w)
	flusher.Flush()

	done := make(chan struct{})
	defer close(done)
	sse.KeepAlive(w, flusher, done, 15*time.Second)

	var lines int
	err = n.Client.Logs(r.Context(), id, engine.LogOptions{Tail: tail, Since: since, Follow: true},
		func(l engine.LogLine) error {
			ev := map[string]string{"stream": l.Stream, "text": l.Text}
			b, merr := jsonMarshal(ev)
			if merr != nil {
				return nil
			}
			if werr := sse.Send(w, sse.Event{Name: "log", Data: b}); werr != nil {
				return werr
			}
			lines++
			// Flush every line: interactive tails are low volume.
			flusher.Flush()
			return nil
		})

	if err != nil && r.Context().Err() == nil {
		payload, _ := jsonMarshal(map[string]string{"error": err.Error()})
		_ = sse.Send(w, sse.Event{Name: "error", Data: payload})
		flusher.Flush()
	}
	_ = sse.Send(w, sse.Event{Name: "close", Data: []byte(`{}`)})
	flusher.Flush()
}

// currentUser resolves the authenticated account for a request.
func currentUser(r *http.Request) *database.User {
	return auth.CurrentUser(r.Context())
}

func currentUsername(r *http.Request) string {
	if u := currentUser(r); u != nil {
		return u.Username
	}
	return ""
}

// seriesResponse is the JSON shape served to chart clients.
type seriesResponse struct {
	NodeID     int64                 `json:"node_id"`
	Container  string                `json:"container"`
	IntervalMS int64                 `json:"interval_ms"`
	Samples    []sse.Sample          `json:"samples"`
	Latest     *sse.ContainerMetrics `json:"latest,omitempty"`
}

// containerSeriesAPI returns the retained series for a container as JSON.
func (s *Server) containerSeriesAPI(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	id := r.PathValue("id")
	if err := engine.ValidateID(id); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	nodeID, ok := s.nodeIDFromRequest(r)
	if !ok {
		jsonError(w, http.StatusNotFound, "no node selected")
		return
	}
	series := s.metrics.Series(nodeID, id)
	resp := seriesResponse{
		NodeID:     nodeID,
		Container:  id,
		IntervalMS: s.metrics.Interval().Milliseconds(),
		Samples:    series,
	}
	if m, ok := s.metrics.Latest(nodeID, id); ok {
		resp.Latest = &m
	}
	writeJSON(w, resp)
}

// containerAction returns the handler for a lifecycle verb.
func (s *Server) containerAction(action string) http.HandlerFunc {
	act := engine.Action(action)
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := engine.ValidateID(id); err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		// Optional body overrides (e.g. signal for kill, timeout for stop).
		_ = r.ParseForm()
		ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
		defer cancel()

		n, ok := s.nodeParam(r)
		if !ok || n == nil || n.Client == nil {
			jsonError(w, http.StatusServiceUnavailable, "node is not connected")
			return
		}
		if !n.Status.Online {
			n = s.ensureOnline(r, n)
		}

		if err := n.Client.Do(ctx, id, act); err != nil {
			status := http.StatusBadGateway
			if errors.Is(err, engine.ErrNotFound) {
				status = http.StatusNotFound
			}
			s.log.Warn("container action failed", "action", action, "container", id, "error", err)
			setHXTrigger(w, "cw:refresh")
			jsonError(w, status, err.Error())
			return
		}
		s.log.Info("container action", "action", action, "container", id, "user", currentUsername(r), "node", n.Record.Name)

		// Refresh the container list and stats for every viewer immediately.
		s.metrics.Trigger(n.Record.ID)
		s.hub.PublishJSON(sse.TopicEvents(n.Record.ID), "containers", map[string]any{
			"node_id": n.Record.ID,
			"ts":      time.Now().UnixMilli(),
			"action":  action,
		})
		setHXTrigger(w, "cw:refresh")
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, map[string]any{"ok": true, "action": action, "id": id})
	}
}

func itoa64(v int64) string {
	u := uint64(v)
	if v < 0 {
		return "-" + itoa64(-v)
	}
	if u == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for u > 0 {
		i--
		b[i] = byte('0' + u%10)
		u /= 10
	}
	return string(b[i:])
}
