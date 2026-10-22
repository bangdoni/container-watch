/* ContainerWatch log viewer.
 * Streams container logs over SSE, strips ANSI escapes, supports pause,
 * client-side search highlighting and export. Registered as the Alpine
 * component factory `cwLogs`.
 */
(function () {
  "use strict";

  const MAX_LINES = 2000;

  // Strip ANSI/VT100 escape sequences (colours, cursor moves, OSC titles).
  const ANSI = /\u001b\[[0-9;?]*[ -/]*[@-~]|\u001b\][^\u0007]*(?:\u0007|\u001b\\)|\u001b[@-Z\\-_]/g;

  const esc = (s) =>
    String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

  const isErrLine = (text) =>
    /\b(error|err|fatal|panic|exception|traceback|failed|failure)\b/i.test(text) ||
    /\b(4\d\d|5\d\d)\b/.test(text);

  // Reads the stream target from the host element's data attributes:
  //   data-stream, data-tail, data-cname
  const fromEl = (el) => ({
    stream: el.dataset.stream || "",
    tail: el.dataset.tail || "200",
    name: el.dataset.cname || "container",
  });

  window.cwLogs = function (opts) {
    // Accept an element (x-data="cwLogs($el)") or a plain object.
    opts = opts && opts.nodeType === 1 ? fromEl(opts) : opts || {};
    return {
      stream: opts.stream || "",
      tail: String(opts.tail || "200"),
      lines: [],
      paused: false,
      follow: true,
      query: "",
      connected: false,
      error: "",
      buffer: [],
      source: null,
      seq: 0,
      _timer: null,

      init() {
        this.connect();
      },

      destroy() {
        this.disconnect();
      },

      connect() {
        this.disconnect();
        if (!this.stream) return;
        const sep = this.stream.indexOf("?") >= 0 ? "&" : "?";
        const url = this.stream + sep + "tail=" + encodeURIComponent(this.tail);
        let src;
        try {
          src = new EventSource(url);
        } catch (err) {
          this.error = "This browser cannot open event streams.";
          return;
        }
        this.source = src;
        src.addEventListener("open", () => {
          this.connected = true;
          this.error = "";
        });
        src.addEventListener("log", (e) => {
          let payload;
          try {
            payload = JSON.parse(e.data || "{}");
          } catch (err) {
            return;
          }
          this.add(payload.text || "");
        });
        src.addEventListener("error", (e) => {
          // A JSON error frame carries detail; a transport error has none.
          if (e && e.data) {
            try {
              const payload = JSON.parse(e.data);
              if (payload.error) this.error = payload.error;
            } catch (err) {
              /* ignore */
            }
            return;
          }
          this.connected = false;
          // EventSource retries automatically; surface a hint after a beat.
          if (this._timer) clearTimeout(this._timer);
          this._timer = setTimeout(() => {
            if (!this.connected && !this.error) this.error = "Stream closed by the server.";
          }, 2500);
        });
        src.addEventListener("close", () => {
          this.connected = false;
          this.error = "Log stream ended. The container may have stopped.";
          this.disconnect();
        });
      },

      disconnect() {
        if (this._timer) {
          clearTimeout(this._timer);
          this._timer = null;
        }
        if (this.source) {
          this.source.close();
          this.source = null;
        }
        this.connected = false;
      },

      add(text) {
        const clean = String(text).replace(ANSI, "").replace(/\r$/, "");
        const n = ++this.seq;
        const line = { id: n, n: n, text: clean, err: isErrLine(clean) };
        if (this.paused) {
          this.buffer.push(line);
          if (this.buffer.length > MAX_LINES) this.buffer.shift();
          return;
        }
        this.push(line);
      },

      push(line) {
        this.lines.push(line);
        if (this.lines.length > MAX_LINES) this.lines.splice(0, this.lines.length - MAX_LINES);
        if (this.follow) this.scrollDown();
      },

      togglePause() {
        this.paused = !this.paused;
        if (!this.paused) {
          const pending = this.buffer;
          this.buffer = [];
          pending.forEach((l) => this.push(l));
          this.scrollDown();
        }
      },

      clear() {
        this.lines = [];
        this.buffer = [];
      },
      restart() {
        this.clear();
        this.error = "";
        this.connect();
      },

      visible() {
        const q = this.query.trim().toLowerCase();
        if (!q) return this.lines;
        return this.lines.filter((l) => l.text.toLowerCase().indexOf(q) >= 0);
      },

      // Returns escaped HTML with matches wrapped in <mark>.
      hl(text) {
        const q = this.query.trim();
        if (!q) return esc(text);
        const lower = text.toLowerCase();
        const needle = q.toLowerCase();
        let out = "";
        let at = 0;
        let idx;
        while ((idx = lower.indexOf(needle, at)) >= 0) {
          out += esc(text.slice(at, idx)) + "<mark>" + esc(text.slice(idx, idx + needle.length)) + "</mark>";
          at = idx + needle.length;
        }
        return out + esc(text.slice(at));
      },

      scrollDown() {
        this.$nextTick(() => {
          const pane = this.$refs.pane;
          if (pane) pane.scrollTop = pane.scrollHeight;
        });
      },

      onScroll() {
        const pane = this.$refs.pane;
        if (!pane) return;
        const atBottom = pane.scrollHeight - pane.scrollTop - pane.clientHeight < 24;
        this.follow = atBottom;
      },

      exportLogs() {
        const rows = this.visible().map((l) => l.text);
        const blob = new Blob([rows.join("\n") + "\n"], { type: "text/plain;charset=utf-8" });
        const url = URL.createObjectURL(blob);
        const a = document.createElement("a");
        a.href = url;
        a.download = (opts.name || "container") + "-logs.txt";
        document.body.appendChild(a);
        a.click();
        a.remove();
        setTimeout(() => URL.revokeObjectURL(url), 2000);
      },

      get count() {
        return this.lines.length;
      },
    };
  };
})();
