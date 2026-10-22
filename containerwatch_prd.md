# Product Requirements Document (PRD): ContainerWatch

**Document Version**: 1.0.0  
**Status**: Draft / Proposed  
**Author**: Engineering Team  
**Last Updated**: October 2026  

---

## 1. Product Overview

**ContainerWatch** is a lightweight, self-hosted, real-time container monitoring application built for Docker and Podman environments. It provides deep observability—including real-time resource utilization, container lifecycles, and streamable logs—across single or distributed nodes (VPS, edge devices, local workstations).

Designed with a high-density, dark-mode aesthetic inspired by **Grafana**, ContainerWatch utilizes a **Golang backend** and an **htmx-driven frontend** to deliver a responsive, Single Page Application (SPA)-like experience with minimal system footprint (<30 MB RAM) and zero heavy JS bundler overhead.

---

## 2. Goals & Key Objectives

1. **Ultra-Low Resource Footprint**: Run comfortably on single-core 512MB RAM VPS instances alongside workloads.
2. **Dual Engine Support**: Seamlessly interface with both Docker (`/var/run/docker.sock`) and Podman (`/run/podman/podman.sock` or REST API) sockets.
3. **Grafana-Inspired Aesthetics**: Offer a high-density, dark UI optimized for system administrators, developers, and homelab users.
4. **Instant Setup**: Deploy via a single binary (`CGO_ENABLED=0`) or a lightweight Docker container (~15MB).
5. **Secure Access Control**: Protect management capabilities with Role-Based Access Control (RBAC) separating regular viewers (`User`) from host operators (`Admin`).

---

## 3. Tech Stack & Architectural Design

| Layer | Technology | Rationale |
| :--- | :--- | :--- |
| **Language & Engine** | Go 1.22+ | Excellent concurrency, native Unix socket communication, static compilation. |
| **Frontend Rendering** | `html/template` + **htmx** | Server-Side Rendered (SSR) interactive UI; eliminates thick frontend frameworks. |
| **Interactivity & UI** | **Alpine.js** + **Tailwind CSS** | Alpine handles lightweight state (modals, dropdowns); Tailwind provides dark theme layout. |
| **Real-Time Data Streaming** | Server-Sent Events (SSE) | Unidirectional event streaming for metrics and logs with auto-reconnection via htmx. |
| **Database** | SQLite (`modernc.org/sqlite`) | CGO-free embedded storage for users, sessions, node connections, and logs buffer. |
| **Runtime Client** | Docker Engine API / Podman REST | Official Go SDKs for direct socket/TCP communication. |

---

## 4. Functional Requirements

### 4.1 Authentication & Role-Based Access Control (RBAC)

- **Authentication System**:
  - Email/Username + Password login with Argon2id password hashing.
  - Cookie-based session tracking (`HttpOnly`, `SameSite=Lax`, `Secure`).
  - First-time setup wizard to create the initial `Admin` account.
  - Option for Admin to enable/disable public user registration.

- **Role Capabilities Matrix**:

| Feature / Action | Guest / Unauthenticated | User Role | Admin Role |
| :--- | :---: | :---: | :---: |
| Access Login / Signup | ✅ | — | — |
| View System Dashboard & Host Stats | ❌ | ✅ | ✅ |
| View Container Details & Live Metrics | ❌ | ✅ | ✅ |
| Stream & Search Container Logs | ❌ | ✅ | ✅ |
| Lifecycle Control (Start/Stop/Restart) | ❌ | ❌ | ✅ |
| Manage Node Sockets / Remote Hosts | ❌ | ❌ | ✅ |
| Manage User Roles & System Settings | ❌ | ❌ | ✅ |

---

### 4.2 Host & Runtime Management

- **Local Discovery**: Automatically detects local Docker (`unix:///var/run/docker.sock`) or Podman (`unix:///run/podman/podman.sock`) sockets on boot.
- **Remote Host Monitoring**: Supports TCP connection strings (`tcp://vps-ip:2376`) with TLS verification for multi-node VPS setups.
- **Node Switcher**: Top-level selector allowing users to switch monitored host target dynamically without reloading the session.

---

### 4.3 Grafana-Inspired UI & Dashboard Views

#### A. Main System Summary Bar
- Real-time overall host CPU utilization gauge.
- Overall RAM usage progress bar.
- Counter cards: **Total Containers**, **Running** (Green), **Paused** (Yellow), **Stopped** (Red).

#### B. High-Density Container List View
- Compact tabular view displaying:
  - **Status**: Visual indicator pill (Running, Exited, Restarting).
  - **Container Name & ID**: Quick-copy ID tag.
  - **Image Name & Tag**: E.g., `redis:7-alpine`.
  - **Resource Sparklines**: Mini live inline charts for CPU and Memory trends.
  - **Port Mapping**: Formatted clickable ports (e.g., `0.0.0.0:8080->80/tcp`).
  - **Uptime / Health Check**: Docker health status if available.
  - **Quick Action Bar**: Buttons for Logs, Metrics, and Lifecycle controls (Admin only).
- Live search/filtering input with debounced `htmx` updates (`hx-get="/containers/search"`).

#### C. Real-Time Metrics Modal / Detail View
- **CPU Metrics**: Percentage load over time line chart.
- **Memory Metrics**: Used vs. Limit in MB/GB with dynamic memory limit alert line.
- **Network I/O**: RX/TX transfer rate in KB/s or MB/s.
- **Disk I/O**: Read/Write throughput operations.
- Data continuously pushed via SSE endpoints (`hx-ext="sse"`).

#### D. Live Container Log Viewer
- **Streaming Log Terminal**: Styled ANSI terminal output (dark background, monospaced font).
- **Line Buffering**: Selector for log history depth (`100`, `500`, `1000`, `5000` lines).
- **Log Controls**:
  - Live Stream Toggle (Pause / Resume auto-scroll).
  - Search & Highlight input (filters stream client-side).
  - ANSI color parsing for structured log visual distinction.
  - Export button to download log buffer as a `.txt` or `.log` file.

---

## 5. UI & Design Guidelines

- **Color Palette (Grafana Dark Aesthetic)**:
  - Primary Background: `#111217` (Dark Charcoal)
  - Secondary Panel/Card: `#181b1f`
  - Border Highlights: `#2c323b`
  - Text Primary: `#d8d9da`
  - Text Muted: `#8e8e8e`
  - Status Colors:
    - Success/Running: `#10b981` (Emerald Green)
    - Warning/Paused: `#f59e0b` (Amber)
    - Danger/Stopped: `#ef4444` (Crimson)
    - Primary Accent: `#38bdf8` (Cyan Blue)
- **Density**: Tight table layouts (`py-1.5 px-3`) maximizing information per pixel without visual clutter.

---

## 6. Non-Functional Requirements

- **Performance**:
  - Dashboard initial load time under **200ms**.
  - Server SSE update footprint: < 1KB per metric push.
  - Go runtime memory usage kept below **30 MB** under active streaming.
- **Security**:
  - Socket path permissions validation.
  - Rate limiting on login routes to prevent brute-force attacks.
  - Strict input sanitization on container action triggers.
- **Deployment & Compatibility**:
  - Supported OS: Linux (Ubuntu, Debian, Alpine, Fedora, Arch), macOS (Docker Desktop / Podman).
  - Distributed as a standalone binary or standard Docker image (`ghcr.io/containerwatch/containerwatch:latest`).

---

## 7. Project File Structure

```text
├── cmd/
│   └── containerwatch/
│       └── main.go              # Application entrypoint
├── internal/
│   ├── config/                  # App environment configuration
│   ├── auth/                    # Argon2id, Sessions, RBAC Middleware
│   ├── database/                # SQLite connection & migrations
│   ├── engine/                  # Docker & Podman client abstractions
│   ├── handlers/                # HTTP & htmx partial handlers
│   └── sse/                     # SSE broker for metrics & logs
├── web/
│   ├── static/                  # Compiled CSS & JS dependencies
│   │   ├── css/tailwind.css
│   │   └── js/htmx.min.js
│   └── templates/               # Go html/templates
│       ├── layouts/             # Base layout template
│       ├── pages/               # Full page views (Login, Dashboard)
│       └── partials/            # htmx dynamic html snippets
├── Dockerfile                   # Multi-stage lightweight build
├── go.mod
└── go.sum
```

---

## 8. Development Roadmap

- **Phase 1 (Core Engine)**:
  - Docker & Podman local socket integration.
  - Container list rendering with htmx.
- **Phase 2 (Auth & Security)**:
  - SQLite integration with user setup, password hashing, and session management.
  - Admin/User role gating on routes.
- **Phase 3 (Metrics & Logs)**:
  - SSE metrics stream implementation.
  - Terminal log streaming viewer with search filter.
- **Phase 4 (Multi-Host & Polish)**:
  - Remote TCP host configuration.
  - UI visual polish matching Grafana dark mode guidelines.