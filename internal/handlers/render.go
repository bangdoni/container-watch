package handlers

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"path"
	"reflect"
	"strings"
	"time"

	"github.com/bangdoni/container-watch/internal/database"
	"github.com/bangdoni/container-watch/internal/version"
	"github.com/bangdoni/container-watch/web"
)

// Renderer loads and executes the embedded templates.
//
// Template files are grouped by folder:
//   - layouts/*.html  : named "layout" definitions (root documents)
//   - partials/*.html : named "{name}" definitions included by pages/layouts
//   - pages/*.html    : named "content" definitions for page bodies
//
// Pages are rendered through the layout defined in pages/{name}.html's
// companion, so each page template declares {{define "content"}}.
type Renderer struct {
	pages    map[string]*template.Template
	partials *template.Template
	funcs    template.FuncMap
}

// funcMap contains the helpers used by templates.
var funcMap = template.FuncMap{
	"formatBytes":    FormatBytes,
	"formatBytesIEC": FormatBytesIEC,
	// rate renders a per-second byte rate; rates are float64 so the int64
	// formatBytesIEC cannot be used directly.
	"rate": func(v float64) string {
		if v < 0 {
			v = 0
		}
		return FormatRate(v)
	},
	"toMB": func(v int64) string { return fmt.Sprintf("%.1f MB", float64(v)/(1024*1024)) },
	"pct":  func(v float64) string { return fmt.Sprintf("%.1f", v) },
	"pctWidth": func(v float64) string {
		if v < 0 {
			v = 0
		}
		if v > 100 {
			v = 100
		}
		return fmt.Sprintf("%.1f%%", v)
	},
	"cpuColor":  CPUColor,
	"memColor":  MemColor,
	"add":       func(a, b int) int { return a + b },
	"sub":       func(a, b int) int { return a - b },
	"seq":       seq,
	"dict":      dict,
	"json":      toJSON,
	"timeAgo":   TimeAgo,
	"shortTime": func(t time.Time) string { return t.Format("15:04:05") },
	"dateTime":  func(t time.Time) string { return t.Format("2006-01-02 15:04") },
	"unixTime": func(sec int64) time.Time {
		if sec <= 0 {
			return time.Time{}
		}
		return time.Unix(sec, 0)
	},
	"duration": shortDuration,
	"splitTag": func(name string) []string {
		if name == "" || name == "<none>" {
			return []string{name, ""}
		}
		if idx := strings.LastIndex(name, ":"); idx > 0 && !strings.Contains(name[idx:], "/") {
			return []string{name[:idx], name[idx+1:]}
		}
		return []string{name, "latest"}
	},
	"len": func(v any) int {
		rv := reflect.ValueOf(v)
		switch rv.Kind() {
		case reflect.Slice, reflect.Map, reflect.Array, reflect.String:
			return rv.Len()
		}
		return 0
	},
	"initial": func(s string) string {
		if s == "" {
			return "?"
		}
		return strings.ToUpper(s[:1])
	},
	"lower": strings.ToLower,
	"title": func(s string) string {
		if s == "" {
			return s
		}
		return strings.ToUpper(s[:1]) + s[1:]
	},
	"truncate": func(n int, s string) string {
		if len(s) <= n {
			return s
		}
		return s[:n] + "…"
	},
	"default": func(def, v string) string {
		if v == "" {
			return def
		}
		return v
	},
	"icon": func(name string) template.HTML { return template.HTML(iconSvg(name)) },
	// asset renders a cache-busted URL for an embedded static file, e.g.
	// {{ asset "js/app.js" }} -> /static/js/app.js?v=1a2b3c4d. Without this a
	// browser keeps a stale bundle for the whole max-age window after an
	// upgrade.
	"asset": assetURL,
	"isAdmin": func(v any) bool {
		switch u := v.(type) {
		case *database.User:
			return u != nil && u.IsAdmin()
		case database.User:
			return u.IsAdmin()
		}
		return false
	},
}

// assetVersions maps a static path ("js/app.js") to a short content hash. It is
// computed once from the embedded filesystem at startup.
var assetVersions = buildAssetVersions()

func buildAssetVersions() map[string]string {
	out := make(map[string]string)
	err := fs.WalkDir(web.Static, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		data, err := web.Static.ReadFile(p)
		if err != nil {
			return nil
		}
		sum := sha256.Sum256(data)
		out[strings.TrimPrefix(p, "static/")] = hex.EncodeToString(sum[:4])
		return nil
	})
	if err != nil {
		return map[string]string{}
	}
	return out
}

// assetURL returns "/static/<path>?v=<hash>" for an embedded asset.
func assetURL(p string) string {
	p = strings.TrimPrefix(strings.TrimPrefix(p, "/"), "static/")
	if v, ok := assetVersions[p]; ok {
		return "/static/" + p + "?v=" + v
	}
	return "/static/" + p
}

// NewRenderer parses all templates.
func NewRenderer() (*Renderer, error) {
	r := &Renderer{pages: make(map[string]*template.Template), funcs: funcMap}

	// Parse every partial and layout into one shared set: both layouts and
	// partials can reference each other.
	base := template.New("root").Funcs(funcMap)
	base, err := base.ParseFS(web.Templates, "templates/partials/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse partials: %w", err)
	}
	base, err = base.ParseFS(web.Templates, "templates/layouts/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse layouts: %w", err)
	}
	r.partials = base

	entries, err := fs.Glob(web.Templates, "templates/pages/*.html")
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		name := strings.TrimSuffix(path.Base(entry), ".html")
		// Clone the shared set so page-level defines don't leak between pages.
		clone, err := base.Clone()
		if err != nil {
			return nil, err
		}
		if _, err := clone.ParseFS(web.Templates, entry); err != nil {
			return nil, fmt.Errorf("parse page %s: %w", name, err)
		}
		r.pages[name] = clone
	}
	return r, nil
}

// Render writes a full page through its layout.
func (r *Renderer) Render(w *bytes.Buffer, page, layout string, data any) error {
	t, ok := r.pages[page]
	if !ok {
		return fmt.Errorf("unknown page %q", page)
	}
	if layout == "" {
		layout = "layout"
	}
	return t.ExecuteTemplate(w, layout, data)
}

// RenderPartial executes a named template (partial or page define) against the
// shared set.
func (r *Renderer) RenderPartial(buf *bytes.Buffer, name string, data any) error {
	if t := r.partials.Lookup(name); t != nil {
		return t.ExecuteTemplate(buf, name, data)
	}
	for _, page := range r.pages {
		if t := page.Lookup(name); t != nil {
			return t.ExecuteTemplate(buf, name, data)
		}
	}
	return fmt.Errorf("unknown template %q", name)
}

// Has reports whether a partial or page define exists.
func (r *Renderer) Has(name string) bool {
	if r.partials.Lookup(name) != nil {
		return true
	}
	for _, page := range r.pages {
		if page.Lookup(name) != nil {
			return true
		}
	}
	return false
}

// --------------------------------------------------------------- helpers ----

// FormatBytes renders a byte count with binary units, e.g. "1.4 GiB".
func FormatBytes(v int64) string {
	return FormatBytesIEC(v)
}

// FormatBytesIEC renders a byte count using KiB/MiB/GiB suffixes.
func FormatBytesIEC(v int64) string {
	const unit = 1024
	if v < unit {
		return fmt.Sprintf("%d B", v)
	}
	div, exp := int64(unit), 0
	for n := v / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	suffixes := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	return fmt.Sprintf("%.1f %s", float64(v)/float64(div), suffixes[exp])
}

// FormatRate renders a per-second byte rate.
func FormatRate(v float64) string {
	return FormatBytesIEC(int64(v)) + "/s"
}

// CPUColor maps a CPU percentage to a threshold colour class.
func CPUColor(p float64) string {
	switch {
	case p >= 90:
		return "text-red-400"
	case p >= 70:
		return "text-amber-400"
	default:
		return "text-emerald-400"
	}
}

// MemColor maps a memory percentage to a threshold colour class.
func MemColor(p float64) string {
	switch {
	case p >= 90:
		return "text-red-400"
	case p >= 75:
		return "text-amber-400"
	default:
		return "text-sky-400"
	}
}

// TimeAgo renders a coarse relative duration.
func TimeAgo(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// shortDuration renders a duration compactly for the settings page.
func shortDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	secs := int64(d.Seconds())
	days := secs / 86400
	hours := (secs % 86400) / 3600
	mins := (secs % 3600) / 60
	parts := make([]string, 0, 3)
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if mins > 0 && days == 0 {
		parts = append(parts, fmt.Sprintf("%dm", mins))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("%ds", secs)
	}
	return strings.Join(parts, " ")
}

// seq returns a slice of ints [0,n).
func seq(n int) []int {
	if n < 0 {
		return nil
	}
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// dict builds a map from alternating key/value pairs for template calls.
func dict(values ...any) (map[string]any, error) {
	if len(values)%2 != 0 {
		return nil, fmt.Errorf("dict requires an even number of arguments")
	}
	m := make(map[string]any, len(values)/2)
	for i := 0; i < len(values); i += 2 {
		k, ok := values[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict keys must be strings")
		}
		m[k] = values[i+1]
	}
	return m, nil
}

func toJSON(v any) template.JS {
	b, err := jsonMarshal(v)
	if err != nil {
		return template.JS("null")
	}
	return template.JS(b)
}

// PageData is embedded in every page/partial view model.
type PageData struct {
	Title     string
	Version   string
	User      *database.User
	CSRF      string
	Active    string
	BuildDate string
}

// Base returns page chrome data for the current request context.
func Base(user *database.User, csrf, active string) PageData {
	return PageData{
		Version:   version.String(),
		User:      user,
		CSRF:      csrf,
		Active:    active,
		BuildDate: version.BuildDate,
	}
}
