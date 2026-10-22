// Package engine is a small, dependency-free client for the Docker Engine API
// (also served by Podman's compat endpoint). It speaks plain HTTP over a unix
// socket or a TLS TCP connection, which keeps ContainerWatch's binary tiny
// while supporting every feature the UI needs.
package engine

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/containerwatch/containerwatch/internal/database"
	"github.com/containerwatch/containerwatch/internal/version"
)

// Kind identifies the container engine flavour.
type Kind string

const (
	KindDocker Kind = "docker"
	KindPodman Kind = "podman"
	KindAuto   Kind = "auto"
)

// String implements fmt.Stringer.
func (k Kind) String() string { return string(k) }

// ErrNotFound is returned when the engine reports a missing object.
var ErrNotFound = errors.New("engine: not found")

// validID guards every identifier interpolated into a URL path. Docker accepts
// names as well as hex ids, so this is permissive but strict enough to prevent
// path traversal and query injection.
var validID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

// ValidateID checks a container identifier before it is used in a request.
func ValidateID(id string) error {
	if !validID.MatchString(id) {
		return errors.New("invalid container identifier")
	}
	return nil
}

// Client is a connection to one engine endpoint.
type Client struct {
	endpoint   string
	kind       Kind
	baseURL    string
	http       *http.Client
	apiVersion string
}

// Options configures a client.
type Options struct {
	Endpoint string
	Kind     Kind
	TLSCA    string
	TLSCert  string
	TLSKey   string
	Timeout  time.Duration
}

// New builds a client for the given endpoint. Endpoint forms:
//
//	unix:///var/run/docker.sock
//	tcp://10.0.0.5:2376       (TLS when certs are supplied)
//	http://10.0.0.5:2375
//	https://10.0.0.5:2376
func New(opts Options) (*Client, error) {
	if opts.Endpoint == "" {
		return nil, errors.New("engine: empty endpoint")
	}
	if opts.Timeout == 0 {
		opts.Timeout = 30 * time.Second
	}
	u, err := url.Parse(opts.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("engine: parse endpoint: %w", err)
	}

	transport := &http.Transport{
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
		DisableCompression:    true,
	}

	c := &Client{
		endpoint: opts.Endpoint,
		kind:     opts.Kind,
		http:     &http.Client{Transport: transport},
	}
	if c.kind == "" {
		c.kind = KindAuto
	}

	switch u.Scheme {
	case "unix":
		path := u.Path
		if path == "" {
			path = u.Opaque
		}
		if path == "" {
			return nil, errors.New("engine: unix endpoint has no socket path")
		}
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}
		c.baseURL = "http://docker"
		c.endpoint = "unix://" + path

	case "npipe":
		return nil, errors.New("engine: named pipes are not supported; use a unix socket or tcp")

	case "tcp", "http", "https":
		host := u.Host
		scheme := "http"
		if u.Scheme == "https" || opts.TLSCA != "" || opts.TLSCert != "" {
			scheme = "https"
			tlsConf, err := tlsConfig(opts)
			if err != nil {
				return nil, err
			}
			transport.TLSClientConfig = tlsConf
		}
		if host == "" {
			return nil, errors.New("engine: endpoint has no host")
		}
		c.baseURL = scheme + "://" + host
		if u.Scheme == "tcp" {
			c.endpoint = "tcp://" + host
		}

	default:
		return nil, fmt.Errorf("engine: unsupported endpoint scheme %q", u.Scheme)
	}

	return c, nil
}

func tlsConfig(opts Options) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if opts.TLSCA != "" {
		pem, err := os.ReadFile(opts.TLSCA)
		if err != nil {
			return nil, fmt.Errorf("engine: read CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("engine: CA file contains no certificates")
		}
		cfg.RootCAs = pool
	}
	if opts.TLSCert != "" || opts.TLSKey != "" {
		cert, err := tls.LoadX509KeyPair(opts.TLSCert, opts.TLSKey)
		if err != nil {
			return nil, fmt.Errorf("engine: load client certificate: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

// Endpoint returns the normalised connection string.
func (c *Client) Endpoint() string { return c.endpoint }

// Kind returns the engine flavour, refined after the first successful probe.
func (c *Client) Kind() Kind { return c.kind }

// APIVersion returns the API version reported by the engine, if known.
func (c *Client) APIVersion() string { return c.apiVersion }

// Info describes an engine and its host, as returned by GET /info.
type Info struct {
	Name              string `json:"Name"`
	ServerVersion     string `json:"ServerVersion"`
	OperatingSystem   string `json:"OperatingSystem"`
	OSType            string `json:"OSType"`
	Architecture      string `json:"Architecture"`
	NCPU              int    `json:"NCPU"`
	MemTotal          int64  `json:"MemTotal"`
	Containers        int    `json:"Containers"`
	ContainersRunning int    `json:"ContainersRunning"`
	ContainersPaused  int    `json:"ContainersPaused"`
	ContainersStopped int    `json:"ContainersStopped"`
	Images            int    `json:"Images"`
	Driver            string `json:"Driver"`
	DockerRootDir     string `json:"DockerRootDir"`
	KernelVersion     string `json:"KernelVersion"`
	// Platform is populated from /version (e.g. "Docker Engine - Community").
	Platform string `json:"-"`
}

type versionResponse struct {
	Version    string `json:"Version"`
	APIVersion string `json:"ApiVersion"`
	Platform   struct {
		Name string `json:"Name"`
	} `json:"Platform"`
	Components []struct {
		Name    string `json:"Name"`
		Version string `json:"Version"`
	} `json:"Components"`
}

// Probe negotiates the API version and detects Docker vs Podman.
func (c *Client) Probe(ctx context.Context) (*Info, error) {
	pctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	var vr versionResponse
	if err := c.getJSON(pctx, "/version", &vr); err != nil {
		return nil, err
	}
	// A zero ApiVersion means the engine answered without version headers;
	// never emit a bare "/v" prefix in that case.
	if vr.APIVersion == "" {
		vr.APIVersion = "v1.41"
	}
	c.apiVersion = vr.APIVersion
	platform := strings.ToLower(vr.Platform.Name)
	for _, comp := range vr.Components {
		platform += " " + strings.ToLower(comp.Name)
		platform += " " + strings.ToLower(comp.Version)
	}
	switch {
	case strings.Contains(platform, "podman"):
		c.kind = KindPodman
	case c.kind == KindAuto:
		c.kind = KindDocker
	}

	var info Info
	if err := c.getJSON(pctx, "/info", &info); err != nil {
		// /info is optional: some restricted sockets only expose /version.
		info = Info{}
	}
	info.Platform = strings.TrimSpace(vr.Platform.Name)
	if info.ServerVersion == "" {
		info.ServerVersion = vr.Version
	}
	return &info, nil
}

// Ping performs a cheap reachability check.
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/_ping", nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", version.UserAgent)
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("engine: ping returned %s", resp.Status)
	}
	return nil
}

// do executes a non-streaming request with a bounded timeout so a hung engine
// cannot pin a handler goroutine forever. Streaming helpers call http.Do
// directly and rely on their context for cancellation.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	// Honour the caller's deadline, but never let a hung engine pin the
	// goroutine forever.
	if _, ok := req.Context().Deadline(); ok {
		return c.http.Do(req)
	}
	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()
	return c.http.Do(req.WithContext(ctx))
}

func (c *Client) url(path string, query url.Values) string {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

func (c *Client) newRequest(ctx context.Context, method, path string, query url.Values, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.url(path, query), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", version.UserAgent)
	req.Header.Set("Accept", "application/json")
	return req, nil
}

func (c *Client) getJSON(ctx context.Context, path string, dst any) error {
	req, err := c.newRequest(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return fmt.Errorf("engine: %s: %w", path, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("engine: %s: %s: %s", path, resp.Status, strings.TrimSpace(string(msg)))
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

// FromNode builds a client from a persisted node record.
func FromNode(n *database.Node) (*Client, error) {
	kind := Kind(n.Engine)
	if kind == "" {
		kind = KindAuto
	}
	return New(Options{
		Endpoint: n.Endpoint,
		Kind:     kind,
		TLSCA:    n.TLSCA,
		TLSCert:  n.TLSCert,
		TLSKey:   n.TLSKey,
	})
}
