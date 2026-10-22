package engine

import (
	"context"
	"sort"
	"strings"
)

// Image is a row from GET /images/json.
type Image struct {
	ID          string            `json:"Id"`
	ParentID    string            `json:"ParentId"`
	RepoTags    []string          `json:"RepoTags"`
	RepoDigests []string          `json:"RepoDigests"`
	Created     int64             `json:"Created"`
	Size        int64             `json:"Size"`
	SharedSize  int64             `json:"SharedSize"`
	Labels      map[string]string `json:"Labels"`
	Containers  int               `json:"Containers"`
}

// ShortID returns the 12-character image id used by the UI.
func (i *Image) ShortID() string {
	id := strings.TrimPrefix(i.ID, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// Repository returns the first repo:tag, or "<none>" for dangling images.
func (i *Image) Repository() string {
	if len(i.RepoTags) == 0 || i.RepoTags[0] == "" || i.RepoTags[0] == "<none>:<none>" {
		return "<none>"
	}
	return i.RepoTags[0]
}

// Name splits Repository into the repository name and tag.
func (i *Image) Name() (repo, tag string) {
	r := i.Repository()
	if r == "<none>" {
		return r, ""
	}
	if idx := strings.LastIndex(r, ":"); idx > 0 && !strings.Contains(r[idx:], "/") {
		return r[:idx], r[idx+1:]
	}
	return r, "latest"
}

// ListImages returns every image on the node, newest first.
func (c *Client) ListImages(ctx context.Context) ([]Image, error) {
	var out []Image
	if err := c.getJSON(ctx, "/images/json", &out); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Created > out[b].Created })
	return out, nil
}
