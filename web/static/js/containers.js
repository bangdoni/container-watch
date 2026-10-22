/* ContainerWatch — shared Alpine component for a container's live actions.
 * Used by the container table rows and the container detail page.
 */
(function () {
  "use strict";

  const post = async (url, body) => {
    const m = document.cookie.match(/(?:^|;\s*)cw_csrf=([^;]+)/);
    const token = m ? decodeURIComponent(m[1]) : "";
    const res = await fetch(url, {
      method: "POST",
      headers: { "X-CSRF-Token": token, "Content-Type": "application/x-www-form-urlencoded" },
      body: body || "",
    });
    if (!res.ok) {
      const text = (await res.text()).trim();
      throw new Error(text || "Request failed (" + res.status + ")");
    }
    return res;
  };

  // Reads container identity from the host element's data attributes:
  //   data-node, data-cid, data-cname
  const fromEl = (el) => ({
    node: el.dataset.node || "",
    id: el.dataset.cid || "",
    name: el.dataset.cname || "",
  });

  window.cwContainer = function (opts) {
    // Accept an element (x-data="cwContainer($el)") or a plain object.
    opts = opts && opts.nodeType === 1 ? fromEl(opts) : opts || {};
    // Every endpoint must be scoped to the node the container lives on.
    const qs = opts.node ? "?node=" + encodeURIComponent(opts.node) : "";
    return {
      busy: "",
      nodeId: opts.node || null,
      cid: opts.id || "",
      cname: opts.name || "",
      get metrics() {
        return Alpine.store("metrics").get(this.cid);
      },
      get cpu() {
        return this.metrics ? this.metrics.cpu : 0;
      },
      get mem() {
        return this.metrics ? this.metrics.mem : 0;
      },
      get memLimit() {
        return this.metrics ? this.metrics.mem_limit : 0;
      },
      get memPct() {
        return this.memLimit ? (this.mem / this.memLimit) * 100 : 0;
      },
      async act(action) {
        if (action === "stop" || action === "kill") {
          Alpine.store("ui").close();
        }
        this.busy = action;
        try {
          await post("/containers/" + encodeURIComponent(this.cid) + "/" + action + qs);
          Alpine.store("ui").toast(this.cname + ": " + action + " sent", "success");
        } catch (err) {
          Alpine.store("ui").toast(err.message || "Action failed", "error");
        } finally {
          this.busy = "";
        }
      },
      openLogs() {
        Alpine.store("ui").open("logs", {
          url: "/containers/" + encodeURIComponent(this.cid) + "/logs-panel" + qs,
          size: "modal-xl",
        });
      },
      openMetrics() {
        Alpine.store("ui").open("metrics", {
          url: "/containers/" + encodeURIComponent(this.cid) + "/metrics-panel" + qs,
          size: "modal-lg",
        });
      },
      openInspect() {
        Alpine.store("ui").open("inspect", {
          url: "/containers/" + encodeURIComponent(this.cid) + "/detail" + qs,
          size: "modal-lg",
        });
      },
    };
  };
})();
