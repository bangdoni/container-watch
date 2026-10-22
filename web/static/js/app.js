/* ==========================================================================
   ContainerWatch client runtime
   - htmx lifecycle wiring (CSRF header, error toasts, OOB config)
   - Alpine stores for live metrics received over SSE
   - Canvas sparklines + SVG charts (no charting dependency)
   ========================================================================== */
(function () {
  "use strict";

  // ---------------------------------------------------------------- utils --
  const fmtBytes = (v) => {
    v = Number(v) || 0;
    if (v < 1024) return v + " B";
    const units = ["KiB", "MiB", "GiB", "TiB"];
    let i = -1;
    do {
      v /= 1024;
      i++;
    } while (v >= 1024 && i < units.length - 1);
    return v.toFixed(v >= 100 ? 0 : v >= 10 ? 1 : 2) + " " + units[i];
  };

  const fmtRate = (v) => fmtBytes(Math.round(Number(v) || 0)) + "/s";

  const fmtPct = (v) => (Number(v) || 0).toFixed(1) + "%";

  const shortNum = (v) => {
    v = Number(v) || 0;
    if (v >= 1e9) return (v / 1e9).toFixed(1) + "G";
    if (v >= 1e6) return (v / 1e6).toFixed(1) + "M";
    if (v >= 1e3) return (v / 1e3).toFixed(1) + "k";
    return String(Math.round(v));
  };

  const esc = (s) =>
    String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

  const cssVar = (name) => getComputedStyle(document.documentElement).getPropertyValue(name).trim();

  window.CW = window.CW || {};
  Object.assign(window.CW, { fmtBytes, fmtRate, fmtPct, shortNum, esc, cssVar });

  // --------------------------------------------------------------- toasts --
  const toasts = {
    host() {
      let el = document.getElementById("toasts");
      if (!el) {
        el = document.createElement("div");
        el.id = "toasts";
        el.className = "toasts";
        el.setAttribute("aria-live", "polite");
        document.body.appendChild(el);
      }
      return el;
    },
    show(message, kind) {
      const el = this.host();
      const t = document.createElement("div");
      t.className = "toast " + (kind || "");
      const icon =
        kind === "error"
          ? '<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="#f2495c" stroke-width="2" stroke-linecap="round"><circle cx="12" cy="12" r="10"/><line x1="12" y1="8" x2="12" y2="13"/><line x1="12" y1="16.5" x2="12.01" y2="16.5"/></svg>'
          : kind === "success"
            ? '<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="#73bf69" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><polyline points="16 9 11 15 8 12"/></svg>'
            : "";
      t.innerHTML = icon + '<div class="flex-1">' + esc(message) + "</div>";
      el.appendChild(t);
      setTimeout(() => {
        t.style.transition = "opacity .25s ease, transform .25s ease";
        t.style.opacity = "0";
        t.style.transform = "translateY(4px)";
        setTimeout(() => t.remove(), 260);
      }, kind === "error" ? 5200 : 3200);
    },
  };
  window.CW.toasts = toasts;

  // ------------------------------------------------------ htmx integration --
  function csrfToken() {
    const m = document.cookie.match(/(?:^|;\s*)cw_csrf=([^;]+)/);
    return m ? decodeURIComponent(m[1]) : "";
  }

  document.addEventListener("htmx:configRequest", (e) => {
    e.detail.headers["X-CSRF-Token"] = csrfToken();
  });

  // Allow SSE payloads to update Alpine stores / trigger refreshes.
  document.addEventListener("htmx:sseMessage", (e) => {
    // Handled per-element via hx-ext="sse" swap semantics; nothing global here.
  });

  document.addEventListener("htmx:responseError", (e) => {
    const xhr = e.detail.xhr;
    const msg = (xhr && xhr.responseText) || "";
    if (xhr && xhr.status === 401) {
      window.location.href = "/login";
      return;
    }
    toasts.show(msg.trim().slice(0, 300) || "Request failed (" + (xhr ? xhr.status : "?") + ")", "error");
  });

  document.addEventListener("htmx:sendError", () => {
    toasts.show("Network error — is the ContainerWatch server still running?", "error");
  });

  document.addEventListener("htmx:afterSwap", (e) => {
    // Re-observe any live charts inside swapped fragments.
    if (window.CW.rehydrate) window.CW.rehydrate(e.detail.target);
  });

  // ------------------------------------------------------- Alpine stores --
  document.addEventListener("alpine:init", () => {
    // Live container metrics keyed by container id, filled by SSE.
    Alpine.store("metrics", {
      data: {},
      nodeId: null,
      snap(evt) {
        try {
          const payload = JSON.parse(evt.detail.data || "{}");
          const map = {};
          (payload.containers || []).forEach((c) => (map[c.id] = c));
          this.data = map;
          this.nodeId = payload.node_id;
          window.dispatchEvent(new CustomEvent("cw:metrics", { detail: payload }));
        } catch (err) {
          /* ignore malformed frame */
        }
      },
      get(id) {
        return this.data[id] || null;
      },
    });

    // Host summary state, updated by the dashboard SSE stream.
    Alpine.store("host", {
      cpu: 0,
      memPct: 0,
      memUsed: 0,
      memTotal: 0,
      load: 0,
      series: [],
      update(evt) {
        try {
          const p = JSON.parse(evt.detail.data || "{}");
          this.cpu = p.cpu || 0;
          this.memPct = p.mem_pct || 0;
          this.memUsed = p.mem_used || 0;
          this.memTotal = p.mem_total || 0;
          this.load = p.load || 0;
          this.series.push(this.cpu);
          if (this.series.length > 60) this.series.shift();
        } catch (err) {
          /* ignore */
        }
      },
    });

    // Global UI store: modal stack, node selector, toasts.
    Alpine.store("ui", {
      modal: null, // { name, title, ... }
      sidebarOpen: false,
      nodeId: null,
      open(name, data) {
        this.modal = Object.assign({ name: name }, data || {});
      },
      close() {
        this.modal = null;
      },
      toast(msg, kind) {
        toasts.show(msg, kind);
      },
    });
  });

  // ------------------------------------------------------- Copy to clipboard --
  document.addEventListener("click", async (e) => {
    const el = e.target.closest("[data-copy]");
    if (!el) return;
    e.preventDefault();
    const text = el.getAttribute("data-copy");
    try {
      await navigator.clipboard.writeText(text);
      toasts.show("Copied " + (text.length > 24 ? text.slice(0, 24) + "…" : text), "success");
    } catch (err) {
      toasts.show("Clipboard unavailable", "error");
    }
  });

  // ---------------------------------------------------------- sparklines --
  // Draws a mini area chart from a numeric array on a canvas, sized to DPR.
  const sparkRegistry = new WeakMap();

  function drawSpark(canvas, values, color, fill) {
    const dpr = window.devicePixelRatio || 1;
    const rect = canvas.getBoundingClientRect();
    const w = Math.max(24, Math.round(rect.width * dpr));
    const h = Math.max(12, Math.round(rect.height * dpr));
    if (canvas.width !== w || canvas.height !== h) {
      canvas.width = w;
      canvas.height = h;
    }
    const ctx = canvas.getContext("2d");
    ctx.clearRect(0, 0, w, h);
    if (!values || values.length < 2) return;

    let min = Math.min.apply(null, values);
    let max = Math.max.apply(null, values);
    if (max - min < 0.5) {
      max = min + 0.5;
    }
    const pad = 1.5 * dpr;
    const n = values.length;
    const px = (i) => (i / (n - 1)) * w;
    const py = (v) => h - pad - ((v - min) / (max - min)) * (h - pad * 2);

    if (fill) {
      const grad = ctx.createLinearGradient(0, 0, 0, h);
      grad.addColorStop(0, fill);
      grad.addColorStop(1, "rgba(0,0,0,0)");
      ctx.beginPath();
      ctx.moveTo(0, h);
      values.forEach((v, i) => ctx.lineTo(px(i), py(v)));
      ctx.lineTo(w, h);
      ctx.closePath();
      ctx.fillStyle = grad;
      ctx.fill();
    }

    ctx.beginPath();
    values.forEach((v, i) => (i === 0 ? ctx.moveTo(px(i), py(v)) : ctx.lineTo(px(i), py(v))));
    ctx.strokeStyle = color;
    ctx.lineWidth = 1.4 * dpr;
    ctx.lineJoin = "round";
    ctx.stroke();

    // Endpoint dot.
    const lx = px(n - 1);
    const ly = py(values[n - 1]);
    ctx.beginPath();
    ctx.arc(lx, ly, 1.8 * dpr, 0, Math.PI * 2);
    ctx.fillStyle = color;
    ctx.fill();
  }

  function colorFor(pct, kind) {
    if (kind === "mem") {
      if (pct >= 90) return "#f2495c";
      if (pct >= 75) return "#ff9830";
      return "#5794f2";
    }
    if (pct >= 90) return "#f2495c";
    if (pct >= 70) return "#fade2a";
    return "#73bf69";
  }

  function fillFor(color) {
    const map = {
      "#f2495c": "rgba(242,73,92,.28)",
      "#ff9830": "rgba(255,152,48,.28)",
      "#fade2a": "rgba(250,222,42,.24)",
      "#73bf69": "rgba(115,191,105,.24)",
      "#5794f2": "rgba(87,148,242,.26)",
    };
    return map[color] || "rgba(110,159,255,.2)";
  }

  // Sparkline elements declare data-spark="cpu|mem|io" and data-id="<container>".
  // Values are pushed by the metrics store subscription below.
  const sparkSeries = {}; // key -> array

  function sparkKey(el) {
    const id = el.getAttribute("data-id") || "";
    const kind = el.getAttribute("data-spark") || "cpu";
    // Tolerate legacy seeds written as "<id>:<kind>".
    return id.endsWith(":" + kind) ? id : id + ":" + kind;
  }

  function pushSpark(key, value, maxLen) {
    const arr = sparkSeries[key] || (sparkSeries[key] = []);
    arr.push(value);
    if (arr.length > (maxLen || 40)) arr.shift();
  }

  function paintSparks(root) {
    (root || document).querySelectorAll("canvas[data-spark]").forEach((canvas) => {
      const key = sparkKey(canvas);
      const kind = canvas.getAttribute("data-spark");
      const values = sparkSeries[key];
      const last = values && values.length ? values[values.length - 1] : 0;
    const color = colorFor(kind === "mem" ? (canvas.dataset.pct || 0) * 1 : last, kind);
      drawSpark(canvas, values, color, fillFor(color));
    });
  }

  // Applies server-provided history to sparklines and charts. Called on load
  // and after every htmx swap, so fragments arrive pre-populated.
  function loadSeeds(root) {
    root.querySelectorAll("script[type='application/json'][data-chart-seed]").forEach((el) => {
      try {
        const seed = JSON.parse(el.textContent || "{}");
        if (seed && seed.key && Array.isArray(seed.series)) {
          window.CW.chartData[seed.key] = seed.series;
        }
      } catch (err) {
        /* ignore */
      }
    });
    root.querySelectorAll("script[type='application/json'][data-seed]").forEach((el) => {
      try {
        const seed = JSON.parse(el.textContent || "{}");
        Object.keys(seed).forEach((key) => {
          (seed[key] || []).forEach((v) => pushSpark(key, v, 40));
        });
      } catch (err) {
        /* ignore */
      }
    });
  }

  window.CW.rehydrate = function (root) {
    root = root || document;
    loadSeeds(root);
    paintSparks(root);
    root.querySelectorAll("[data-chart]").forEach(renderChart);
  };

  // On metrics updates, feed the spark series + repaint.
  window.addEventListener("cw:metrics", (e) => {
    const payload = e.detail || {};
    (payload.containers || []).forEach((c) => {
      pushSpark(c.id + ":cpu", c.cpu, 40);
      pushSpark(c.id + ":mem", c.mem_limit ? (c.mem / c.mem_limit) * 100 : 0, 40);
    });
    paintSparks(document);
    // Update live text cells bound by data-live attributes.
    document.querySelectorAll("[data-live]").forEach((el) => {
      const id = el.getAttribute("data-live-id");
      const field = el.getAttribute("data-live");
      const m = Alpine.store("metrics").get(id);
      if (!m) return;
      if (field === "cpu") el.textContent = fmtPct(m.cpu);
      else if (field === "mem") el.textContent = fmtBytes(m.mem);
      else if (field === "net") el.textContent = "↓ " + fmtRate(m.rx_rate) + "  ↑ " + fmtRate(m.tx_rate);
    });
  });

  // ------------------------------------------------------------ SVG charts --
  // A chart element declares:
  //   data-chart="cpu|mem|io"
  //   data-id="<container id>" data-node="<node id>"
  // Values are read from window.CW.chartData[key] (seeded server-side) and then
  // appended from the live metrics event.
  window.CW.chartData = window.CW.chartData || {};

  function renderChart(svg) {
    const kind = svg.getAttribute("data-chart");
    const key = svg.getAttribute("data-chart-key");
    const series = window.CW.chartData[key] || [];
    const rect = svg.viewBox.baseVal;
    const W = rect && rect.width ? rect.width : 320;
    const H = rect && rect.height ? rect.height : 74;
    let fmt = (v) => v;
    let max = 1;
    let values;
    if (kind === "mem") {
      values = series.map((s) => s.mem);
      max = Math.max.apply(null, series.map((s) => Math.max(s.limit || 0, s.mem || 0)).concat([1]));
      fmt = (v) => fmtBytes(v);
    } else if (kind === "io") {
      values = series.map((s) => (s.net_rx || 0) + (s.net_tx || 0));
      max = Math.max.apply(null, values.concat([1]));
      fmt = (v) => fmtBytes(v);
    } else if (kind === "blk") {
      values = series.map((s) => (s.blk_read || 0) + (s.blk_write || 0));
      max = Math.max.apply(null, values.concat([1]));
      fmt = (v) => fmtBytes(v);
    } else {
      values = series.map((s) => s.cpu);
      max = Math.max(100, Math.max.apply(null, values.concat([0])) * 1.15);
      fmt = (v) => fmtPct(v);
    }
    const color =
      kind === "cpu" ? "#73bf69" : kind === "mem" ? "#5794f2" : kind === "blk" ? "#ff9830" : "#b877d9";
    const n = values.length;
    const padTop = 4;
    const plotH = H - padTop - 2;
    const px = (i) => (n <= 1 ? 0 : (i / (n - 1)) * W);
    const py = (v) => padTop + plotH - (Math.max(0, v) / max) * plotH;

    let d = "";
    if (n >= 2) {
      values.forEach((v, i) => (d += (i ? "L" : "M") + px(i).toFixed(1) + " " + py(v).toFixed(1)));
    }
    const area = n >= 2 ? d + "L" + W + " " + (padTop + plotH) + "L0 " + (padTop + plotH) + "Z" : "";
    const last = n ? values[n - 1] : 0;

    svg.innerHTML = [
      '<line class="grid-line" x1="0" y1="' + padTop + '" x2="' + W + '" y2="' + padTop + '"/>',
      '<line class="grid-line" x1="0" y1="' + (padTop + plotH / 2) + '" x2="' + W + '" y2="' + (padTop + plotH / 2) + '"/>',
      '<line class="grid-line" x1="0" y1="' + (padTop + plotH) + '" x2="' + W + '" y2="' + (padTop + plotH) + '"/>',
      area ? '<path class="area" d="' + area + '" fill="' + color + '"/>' : "",
      d ? '<path class="line" d="' + d + '" stroke="' + color + '"/>' : "",
      n
        ? '<circle cx="' + px(n - 1).toFixed(1) + '" cy="' + py(last).toFixed(1) + '" r="2.4" fill="' + color + '"/>'
        : "",
      '<text class="axis" x="2" y="' + (H - 0.5) + '">' + esc(fmt(max)) + "</text>",
    ].join("");
  }

  window.addEventListener("cw:metrics", (e) => {
    const payload = e.detail || {};
    const nodeId = payload.node_id;
    (payload.containers || []).forEach((c) => {
      const key = nodeId + "/" + c.id;
      const arr = window.CW.chartData[key] || (window.CW.chartData[key] = []);
      arr.push(c);
      if (arr.length > 90) arr.shift();
    });
    document.querySelectorAll("svg[data-chart]").forEach((svg) => {
      const k = svg.getAttribute("data-chart-key");
      if (!k || k.indexOf(String(nodeId) + "/") !== 0) return;
      renderChart(svg);
    });
    // Update chart headline values.
    (payload.containers || []).forEach((c) => {
      document.querySelectorAll('[data-chart-value="' + c.id + '"]').forEach((el) => {
        const kind = el.getAttribute("data-kind");
        if (kind === "cpu") el.textContent = fmtPct(c.cpu);
        else if (kind === "mem") el.textContent = fmtBytes(c.mem);
        else if (kind === "io") el.textContent = "↓" + fmtRate(c.rx_rate) + " ↑" + fmtRate(c.tx_rate);
        else if (kind === "blk") el.textContent = "R" + fmtRate(c.read_rate) + " W" + fmtRate(c.write_rate);
      });
    });
  });

  // Kick off initial paint once the DOM is ready.
  document.addEventListener("DOMContentLoaded", () => {
    window.CW.rehydrate(document);
  });

  // --------------------------------------------------------- modal helpers --
  window.CW.openModal = function (name, url, data) {
    Alpine.store("ui").open(name, Object.assign({ url: url }, data || {}));
  };
  window.CW.closeModal = function () {
    Alpine.store("ui").close();
  };

  // Close modal on Escape.
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape") {
      if (Alpine.store("ui") && Alpine.store("ui").modal) {
        e.preventDefault();
        Alpine.store("ui").close();
      }
    }
    // "/" focuses the container search.
    if (e.key === "/" && !/^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement.tagName)) {
      const s = document.getElementById("container-search");
      if (s) {
        e.preventDefault();
        s.focus();
        s.select();
      }
    }
  });
})();
