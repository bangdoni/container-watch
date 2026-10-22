package handlers

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/containerwatch/containerwatch/internal/engine"
)

// row is the view model for one line of the container table. It precomputes the
// values templates need so the markup stays free of arithmetic.
type row struct {
	C *engine.Container

	Name    string
	Short   string
	Image   string
	Command string
	State   string
	Status  string
	Health  string
	Uptime  string
	Ports   []engine.Port

	CPU      float64
	Mem      int64
	MemLimit int64
	MemPct   float64
	HasStats bool

	RxRate float64
	TxRate float64

	// Accent drives the state pill colour.
	Accent string
	// Sparks holds the sparkline seed values.
	Sparks []float64
}

// Running reports whether the container is up.
func (r row) Running() bool { return r.State == "running" }

// Busy reports whether the container is transitioning.
func (r row) Busy() bool { return r.State == "restarting" || r.State == "removing" }

// Paused reports whether the container is paused.
func (r row) Paused() bool { return r.State == "paused" }

// stateAccent maps an engine state to a palette suffix.
func stateAccent(state string) string {
	switch state {
	case "running":
		return "green"
	case "paused":
		return "yellow"
	case "restarting", "created":
		return "blue"
	case "exited", "dead":
		return "red"
	default:
		return "gray"
	}
}

// buildRows augments containers with their live metrics and state colouring.
func (s *Server) buildRows(nodeID int64, containers []engine.Container) []row {
	out := make([]row, 0, len(containers))
	for i := range containers {
		c := &containers[i]
		r := row{
			C:       c,
			Name:    c.Name(),
			Short:   c.ShortID(),
			Image:   c.Image,
			Command: c.Command,
			State:   c.State,
			Status:  c.Status,
			Health:  c.Health(),
			Uptime:  c.Uptime(),
			Ports:   c.Ports,
			Accent:  stateAccent(c.State),
		}
		if m, ok := s.metrics.Latest(nodeID, c.ID); ok {
			r.CPU = m.CPU
			r.Mem = m.Mem
			r.MemLimit = m.MemLimit
			r.MemPct = m.MemPercent()
			r.RxRate = m.RxRate
			r.TxRate = m.TxRate
			r.HasStats = true
		}
		out = append(out, r)
	}
	return out
}

// seed builds the sparkline seed map consumed by app.js: "<id>:cpu" / ":mem".
func (s *Server) seed(nodeID int64, containers []engine.Container) map[string][]float64 {
	out := make(map[string][]float64, len(containers)*2)
	for _, c := range containers {
		series := s.metrics.Series(nodeID, c.ID)
		if len(series) == 0 {
			continue
		}
		cpu := make([]float64, 0, len(series))
		mem := make([]float64, 0, len(series))
		for _, p := range series {
			cpu = append(cpu, p.CPU)
			if p.Limit > 0 {
				mem = append(mem, float64(p.Mem)/float64(p.Limit)*100)
			} else {
				mem = append(mem, 0)
			}
		}
		out[c.ID+":cpu"] = cpu
		out[c.ID+":mem"] = mem
	}
	return out
}

// filterRows applies the dashboard search box (name, image, id, state).
func filterRows(rows []row, q string) []row {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return rows
	}
	out := make([]row, 0, len(rows))
	for _, r := range rows {
		if strings.Contains(strings.ToLower(r.Name), q) ||
			strings.Contains(strings.ToLower(r.Image), q) ||
			strings.Contains(strings.ToLower(r.Short), q) ||
			strings.Contains(strings.ToLower(r.State), q) {
			out = append(out, r)
		}
	}
	return out
}

// sortRows orders rows by state (running first) then name, so the dashboard
// summary always leads with what is actually alive.
func sortRows(rows []row) {
	rank := func(r row) int {
		switch r.State {
		case "running":
			return 0
		case "paused":
			return 1
		case "restarting":
			return 2
		default:
			return 3
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		ri, rj := rank(rows[i]), rank(rows[j])
		if ri != rj {
			return ri < rj
		}
		return strings.ToLower(rows[i].Name) < strings.ToLower(rows[j].Name)
	})
}

// summary aggregates the counts shown in the dashboard summary bar.
type summary struct {
	Total     int
	Running   int
	Paused    int
	Stopped   int
	Unhealthy int
	CPU       float64
	Mem       int64
	MemLimit  int64
	MemPct    float64
	RxRate    float64
	TxRate    float64
}

// summarise aggregates container counts and resource totals.
func summarise(rows []row) summary {
	var sm summary
	for _, r := range rows {
		sm.Total++
		switch r.State {
		case "running":
			sm.Running++
		case "paused":
			sm.Paused++
			sm.Running++
		default:
			sm.Stopped++
		}
		if r.Health == "unhealthy" {
			sm.Unhealthy++
		}
		if r.MemLimit > sm.MemLimit {
			sm.MemLimit = r.MemLimit
		}
		sm.CPU += r.CPU
		sm.Mem += r.Mem
		sm.RxRate += r.RxRate
		sm.TxRate += r.TxRate
	}
	if sm.MemLimit > 0 {
		sm.MemPct = float64(sm.Mem) / float64(sm.MemLimit) * 100
	}
	return sm
}

// hostView is the host panel: it prefers real host readings (local node) and
// falls back to engine-reported totals for remote nodes.
type hostView struct {
	Available  bool
	CPU        float64
	MemUsed    int64
	MemTotal   int64
	MemPct     float64
	Load       float64
	Source     string
	Engine     string
	Platform   string
	Version    string
	CPUs       int
	Containers int
	Images     int
	Uptime     string
}

// hostFor samples the host behind a node. Local nodes are measured from procfs
// or sysctl; remote nodes report what the engine says about itself.
func (s *Server) hostFor(n *engine.Node, sm summary) hostView {
	hv := hostView{
		Engine:   string(n.Status.Kind),
		Platform: n.Status.Platform,
		Version:  n.Status.Version,
		CPUs:     n.Status.CPUs,
		Source:   "engine",
	}
	if n.Status.Kind == "" {
		hv.Engine = n.Record.Engine
	}
	if n.Status.Local {
		hs := s.host.Sample()
		if hs.MemTotal > 0 {
			hv.Available = true
			hv.CPU = hs.CPUPercent
			hv.MemUsed = hs.MemUsed
			hv.MemTotal = hs.MemTotal
			hv.MemPct = hs.MemUsedPercent()
			hv.Load = hs.LoadAvg
			hv.Source = hs.Source
			return hv
		}
	}
	// Remote: derive from the engine's memory total and the containers' usage.
	if n.Status.MemTotal > 0 {
		hv.Available = true
		hv.MemTotal = n.Status.MemTotal
		hv.MemUsed = sm.Mem
		hv.MemPct = float64(sm.Mem) / float64(n.Status.MemTotal) * 100
		hv.CPU = sm.CPU
	}
	return hv
}

// dashboardView is the payload for the dashboard page.
type dashboardView struct {
	viewData
	Summary    summary
	Containers []row
	Top        []row
	Host       hostView
	Seed       map[string][]float64
	Query      string
	ServerVer  *engine.NodeStatus
	Started    time.Time
	// Filter mirrors tablePartialView.Filter so the shared table partial
	// renders identically on the dashboard and after an htmx swap.
	Filter string
	// NodeUser is the current user, exposed for the shared table partial.
	NodeUser any
}

// dashboardPage renders the summary bar plus the live container table.
func (s *Server) dashboardPage(w http.ResponseWriter, r *http.Request) {
	vd, n, ok := s.page(w, r, "dashboard", "Dashboard")
	if !ok || n == nil {
		s.renderPage(w, r, http.StatusOK, "dashboard", "layout", dashboardView{
			viewData:  vd,
			Started:   s.started,
			ServerVer: nil,
		})
		return
	}
	vd.Title = "Dashboard"

	containers, err := s.containersFor(r, n)
	if err != nil {
		vd.Flash = "Cannot reach " + n.Record.Name + ": " + err.Error()
		vd.FlashKind = "error"
	}
	rows := s.buildRows(n.Record.ID, containers)
	sortRows(rows)
	sm := summarise(rows)

	view := dashboardView{
		viewData:   vd,
		Summary:    sm,
		Containers: rows,
		Host:       s.hostFor(n, sm),
		Seed:       s.seed(n.Record.ID, containers),
		Started:    s.started,
		Filter:     "all",
		NodeUser:   userOf(r),
	}
	st := n.Status
	view.ServerVer = &st
	view.Top = topByCPU(rows, 4)
	s.renderPage(w, r, http.StatusOK, "dashboard", "layout", view)
}

// topByCPU returns the busiest running containers for the "hot" list.
func topByCPU(rows []row, n int) []row {
	live := make([]row, 0, len(rows))
	for _, r := range rows {
		if r.State == "running" {
			live = append(live, r)
		}
	}
	sort.SliceStable(live, func(i, j int) bool { return live[i].CPU > live[j].CPU })
	if len(live) > n {
		live = live[:n]
	}
	return live
}

// containersView is the payload for the containers list page.
type containersView struct {
	viewData
	Summary    summary
	Containers []row
	Seed       map[string][]float64
	Query      string
	Filter     string
	NodeUser   any
	// NodeError carries the last connection error for the selected node.
	// The shared "containers-table" partial renders it in the unreachable
	// state so the operator sees why the engine could not be reached
	// instead of an empty table that looks like "no containers".
	NodeError string
}

// containersPage renders the full container list.
func (s *Server) containersPage(w http.ResponseWriter, r *http.Request) {
	vd, n, ok := s.page(w, r, "containers", "Containers")
	if !ok || n == nil {
		s.renderPage(w, r, http.StatusOK, "containers", "layout", containersView{viewData: vd})
		return
	}
	containers, err := s.containersFor(r, n)
	if err != nil {
		vd.Flash = "Cannot reach " + n.Record.Name + ": " + err.Error()
		vd.FlashKind = "error"
	}
	nodeErr := firstNonEmpty(errString(err), n.Status.Error)
	rows := s.buildRows(n.Record.ID, containers)
	sortRows(rows)
	q := r.URL.Query().Get("q")
	if flt := r.URL.Query().Get("state"); flt != "" && flt != "all" {
		rows = filterByState(rows, flt)
	}
	s.renderPage(w, r, http.StatusOK, "containers", "layout", containersView{
		viewData:   vd,
		Summary:    summarise(s.buildRows(n.Record.ID, containers)),
		Containers: filterRows(rows, q),
		Seed:       s.seed(n.Record.ID, containers),
		Query:      q,
		Filter:     defaultStr(r.URL.Query().Get("state"), "all"),
		NodeUser:   userOf(r),
		NodeError:  nodeErr,
	})
}

// firstNonEmpty returns the first non-blank value, or "" when all are blank.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// errString returns err.Error() or "" when err is nil.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func defaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// filterByState keeps rows matching a lifecycle filter.
func filterByState(rows []row, state string) []row {
	out := make([]row, 0, len(rows))
	for _, r := range rows {
		switch state {
		case "running":
			if r.State == "running" {
				out = append(out, r)
			}
		case "paused":
			if r.State == "paused" {
				out = append(out, r)
			}
		case "stopped":
			if r.State != "running" && r.State != "paused" && r.State != "restarting" {
				out = append(out, r)
			}
		case "unhealthy":
			if r.Health == "unhealthy" {
				out = append(out, r)
			}
		default:
			out = append(out, r)
		}
	}
	return out
}

// tablePartialView is the payload of the swapped container table fragment.
type tablePartialView struct {
	Containers []row
	NodeID     int64
	Query      string
	Filter     string
	Seed       map[string][]float64
	CSRF       string
	User       any
	// NodeUser is the current user, used by the shared table partial.
	NodeUser any
	Compact  bool
	// NodeOnline mirrors engine.NodeStatus.Online for the selected node.
	// The template needs it to tell "the engine is up but has no containers"
	// apart from "the engine is unreachable" — without it every empty list
	// renders as the former, which is actively misleading when Docker is
	// down.
	NodeOnline bool
	// NodeError carries the last connection error for the node, shown in
	// the unreachable state so the operator knows what actually failed.
	NodeError string
}

// containersTablePartial re-renders the table after an SSE "changed" event or a
// search keystroke.
func (s *Server) containersTablePartial(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	n, ok := s.nodeParam(r)
	if !ok || n == nil {
		s.renderPartial(w, r, "containers-table", tablePartialView{})
		return
	}
	// Probe a stale node once so a page opened against a sleeping host
	// does not immediately render as offline. Same helper the page view
	// uses, so the header pill and the table agree.
	n = s.ensureOnline(r, n)
	cols, err := s.containersFor(r, n)
	if err != nil {
		// The engine could not be reached. Report that state explicitly
		// instead of rendering an empty table, which would read as
		// "reachable but no containers".
		s.renderPartial(w, r, "containers-table", tablePartialView{
			NodeID:     n.Record.ID,
			NodeOnline: n.Status.Online,
			NodeError:  firstNonEmpty(err.Error(), n.Status.Error),
		})
		return
	}
	rows := s.buildRows(n.Record.ID, cols)
	sortRows(rows)
	flt := r.URL.Query().Get("state")
	rows = filterByState(rows, flt)
	q := r.URL.Query().Get("q")
	rows = filterRows(rows, q)
	s.renderPartial(w, r, "containers-table", tablePartialView{
		Containers: rows,
		NodeID:     n.Record.ID,
		Query:      q,
		Filter:     defaultStr(flt, "all"),
		Seed:       s.seed(n.Record.ID, cols),
		CSRF:       s.auth.EnsureCSRF(w, r),
		User:       userOf(r),
		NodeUser:   userOf(r),
		NodeOnline: n.Status.Online,
		NodeError:  n.Status.Error,
	})
}

// containersSearchPartial is a lightweight endpoint used by the search input
// when htmx requests only the row body.
func (s *Server) containersSearchPartial(w http.ResponseWriter, r *http.Request) {
	s.containersTablePartial(w, r)
}

// imagesView is the payload for the images page.
type imagesView struct {
	viewData
	Images []*engine.Image
	Total  int64
}

// imagesPage lists the images available on the selected node.
func (s *Server) imagesPage(w http.ResponseWriter, r *http.Request) {
	vd, n, ok := s.page(w, r, "images", "Images")
	if !ok || n == nil || n.Client == nil {
		s.renderPage(w, r, http.StatusOK, "images", "layout", imagesView{viewData: vd})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if !n.Status.Online {
		if err := s.nodes.ProbeByID(ctx, n.Record.ID); err == nil {
			n, _ = s.nodes.Get(n.Record.ID)
		}
	}
	images, err := n.Client.ListImages(ctx)
	if err != nil {
		vd.Flash = "Cannot list images: " + err.Error()
		vd.FlashKind = "error"
		s.renderPage(w, r, http.StatusOK, "images", "layout", imagesView{viewData: vd})
		return
	}
	var total int64
	out := make([]*engine.Image, 0, len(images))
	for i := range images {
		total += images[i].Size
		out = append(out, &images[i])
	}
	s.renderPage(w, r, http.StatusOK, "images", "layout", imagesView{
		viewData: vd, Images: out, Total: total,
	})
}
