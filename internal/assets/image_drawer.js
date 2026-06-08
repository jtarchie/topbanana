// image_drawer.js — controls the shared Images side-drawer used on the
// workspace, visual-editor, and manage pages. Each page calls TBImageDrawer.init
// once with a {slug, mode, onInsert} object and gets back open()/close()
// methods. The drawer fetches GET /assets/:slug on open, renders a thumbnail
// grid, and routes "Insert" through the host-supplied onInsert callback.
//
// Modes:
//   "insert" — Insert button is shown next to Save in the detail view.
//   "view"   — Insert button is hidden; the drawer is for browsing + editing
//              metadata only (used on the manage page).
//
// The drawer assumes the partial `image_drawer.html` is already rendered into
// the page (it provides the DOM scaffolding this module wires up). All HTTP
// requests carry the session cookie automatically because they're same-origin.
(function () {
  function el(id) { return document.getElementById(id); }
  function $(sel, root) { return (root || document).querySelector(sel); }

  function escapeHTML(s) {
    return String(s == null ? "" : s)
      .replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;").replace(/'/g, "&#39;");
  }

  function formatSize(n) {
    if (!n) return "";
    if (n < 1024) return n + " B";
    if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " KB";
    return (n / (1024 * 1024)).toFixed(1) + " MB";
  }

  function setStatus(msg, kind) {
    var s = el("tb-drawer-status");
    if (!s) return;
    s.textContent = msg || "";
    s.className = "text-xs " + (kind === "err" ? "text-error" : kind === "ok" ? "text-success" : "text-base-content/60");
  }

  function TBImageDrawer() {}

  TBImageDrawer.prototype.init = function (cfg) {
    this.slug = cfg.slug;
    this.mode = cfg.mode || "insert";
    this.onInsert = cfg.onInsert || function () {};
    this.assets = [];
    this.selected = null;
    this.saveTimer = null;

    var self = this;
    var close = el("tb-drawer-close");
    var scrim = el("tb-drawer-scrim");
    var uploadBtn = el("tb-drawer-upload");
    var uploadInput = el("tb-drawer-upload-input");
    var insertBtn = el("tb-drawer-insert");
    var backBtn = el("tb-drawer-back");
    var altIn = el("tb-drawer-detail-alt");
    var descIn = el("tb-drawer-detail-desc");

    if (close) close.addEventListener("click", function () { self.close(); });
    if (scrim) scrim.addEventListener("click", function () { self.close(); });
    if (uploadBtn && uploadInput) {
      uploadBtn.addEventListener("click", function () { uploadInput.click(); });
      uploadInput.addEventListener("change", function () { self.upload(uploadInput.files); });
    }
    if (insertBtn) {
      if (self.mode === "view") {
        insertBtn.hidden = true;
      } else {
        insertBtn.addEventListener("click", function () { self.insertSelected(); });
      }
    }
    if (backBtn) backBtn.addEventListener("click", function () { self.showGrid(); });

    // Autosave: debounce on input (so a fast typist doesn't fire a PATCH per
    // keystroke), and force a flush on blur so the user gets immediate
    // feedback when they tab away. The blur flush also handles the case
    // where the user clicks Insert before the debounce timer fires — the
    // input loses focus, the save runs, then Insert proceeds.
    function scheduleSave() {
      if (self.saveTimer) clearTimeout(self.saveTimer);
      self.saveTimer = setTimeout(function () { self.saveTimer = null; self.saveDetail(); }, 400);
    }
    function flushSave() {
      if (self.saveTimer) { clearTimeout(self.saveTimer); self.saveTimer = null; self.saveDetail(); }
    }
    if (altIn) {
      altIn.addEventListener("input", scheduleSave);
      altIn.addEventListener("blur", flushSave);
    }
    if (descIn) {
      descIn.addEventListener("input", scheduleSave);
      descIn.addEventListener("blur", flushSave);
    }

    document.addEventListener("keydown", function (e) {
      if (e.key !== "Escape" || !self.isOpen()) return;
      // Esc from the detail view goes back to the grid first; another Esc
      // closes the drawer entirely. This matches the user's mental model of
      // "step back" rather than "blow it all up."
      var detail = el("tb-drawer-detail");
      if (detail && !detail.hidden) self.showGrid();
      else self.close();
    });
  };

  TBImageDrawer.prototype.isOpen = function () {
    var panel = el("tb-drawer-panel");
    return panel && panel.dataset.open === "true";
  };

  TBImageDrawer.prototype.open = function () {
    var panel = el("tb-drawer-panel");
    var scrim = el("tb-drawer-scrim");
    if (!panel) return;
    panel.dataset.open = "true";
    panel.setAttribute("aria-hidden", "false");
    if (scrim) scrim.dataset.open = "true";
    this.showGrid();
    this.fetchAssets();
  };

  TBImageDrawer.prototype.close = function () {
    var panel = el("tb-drawer-panel");
    var scrim = el("tb-drawer-scrim");
    if (!panel) return;
    panel.dataset.open = "false";
    panel.setAttribute("aria-hidden", "true");
    if (scrim) scrim.dataset.open = "false";
  };

  TBImageDrawer.prototype.fetchAssets = function () {
    var self = this;
    setStatus("Loading…");
    fetch("/assets/" + self.slug, { credentials: "same-origin" })
      .then(function (r) {
        if (!r.ok) throw new Error("HTTP " + r.status);
        return r.json();
      })
      .then(function (rows) {
        self.assets = rows || [];
        self.renderGrid();
        setStatus(self.assets.length ? "" : "No images yet — use Upload to add one.");
      })
      .catch(function (err) { setStatus("Load failed: " + err, "err"); });
  };

  TBImageDrawer.prototype.renderGrid = function () {
    var grid = el("tb-drawer-grid");
    if (!grid) return;
    if (!this.assets.length) { grid.innerHTML = ""; return; }
    var self = this;
    grid.innerHTML = this.assets.map(function (a, i) {
      var alt = escapeHTML(a.alt || a.path);
      var name = escapeHTML(a.path.replace(/^assets\//, ""));
      return (
        '<button type="button" data-i="' + i + '" class="tb-drawer-card text-left card bg-base-100 border border-base-300 hover:border-primary transition overflow-hidden">' +
        '  <div class="aspect-square bg-base-200 flex items-center justify-center overflow-hidden">' +
        '    <img src="' + escapeHTML(a.url) + '" alt="' + alt + '" class="object-contain w-full h-full" loading="lazy">' +
        '  </div>' +
        '  <div class="p-2">' +
        '    <div class="font-mono text-xs truncate" title="' + escapeHTML(a.path) + '">' + name + '</div>' +
        '    <div class="text-xs text-base-content/60 truncate">' + (a.alt ? escapeHTML(a.alt) : '<span class="italic">no alt</span>') + '</div>' +
        '  </div>' +
        '</button>'
      );
    }).join("");
    Array.prototype.forEach.call(grid.querySelectorAll(".tb-drawer-card"), function (btn) {
      btn.addEventListener("click", function () {
        var i = parseInt(btn.getAttribute("data-i"), 10);
        self.showDetail(self.assets[i]);
      });
    });
  };

  TBImageDrawer.prototype.showGrid = function () {
    var grid = el("tb-drawer-grid-wrap");
    var detail = el("tb-drawer-detail");
    if (grid) grid.hidden = false;
    if (detail) detail.hidden = true;
    this.selected = null;
  };

  TBImageDrawer.prototype.showDetail = function (asset) {
    var grid = el("tb-drawer-grid-wrap");
    var detail = el("tb-drawer-detail");
    if (grid) grid.hidden = true;
    if (detail) detail.hidden = false;
    this.selected = asset;
    var img = el("tb-drawer-detail-img");
    var path = el("tb-drawer-detail-path");
    var size = el("tb-drawer-detail-size");
    var altIn = el("tb-drawer-detail-alt");
    var descIn = el("tb-drawer-detail-desc");
    var backBtn = el("tb-drawer-back");
    if (img) { img.src = asset.url; img.alt = asset.alt || asset.path; }
    if (path) path.textContent = asset.path;
    if (size) size.textContent = formatSize(asset.size);
    if (altIn) altIn.value = asset.alt || "";
    if (descIn) descIn.value = asset.description || "";
    setStatus("");
    // Move focus into the new region so keyboard users land somewhere
    // meaningful instead of staying on the now-hidden card button.
    if (backBtn) { try { backBtn.focus(); } catch (_) { /* ignore */ } }
  };

  TBImageDrawer.prototype.saveDetail = function () {
    if (!this.selected) return;
    var self = this;
    var altIn = el("tb-drawer-detail-alt");
    var descIn = el("tb-drawer-detail-desc");
    var alt = altIn ? altIn.value : "";
    var desc = descIn ? descIn.value : "";
    // Short-circuit no-op PATCHes — autosave fires on every blur, including
    // blurs that crossed an untouched field.
    if (alt === (self.selected.alt || "") && desc === (self.selected.description || "")) return;
    setStatus("Saving…");
    fetch("/assets/" + self.slug + "/" + self.selected.path, {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      credentials: "same-origin",
      body: JSON.stringify({ alt: alt, description: desc }),
    })
      .then(function (r) {
        if (!r.ok) throw new Error("HTTP " + r.status);
        return r.json();
      })
      .then(function (updated) {
        // Reflect the saved value (server may have trimmed/capped).
        var altTrimmed = (updated.alt || "") !== alt;
        var descTrimmed = (updated.description || "") !== desc;
        self.selected.alt = updated.alt;
        self.selected.description = updated.description;
        if (altIn && altIn.value !== (updated.alt || "")) altIn.value = updated.alt || "";
        if (descIn && descIn.value !== (updated.description || "")) descIn.value = updated.description || "";
        for (var i = 0; i < self.assets.length; i++) {
          if (self.assets[i].path === self.selected.path) {
            self.assets[i].alt = updated.alt;
            self.assets[i].description = updated.description;
            break;
          }
        }
        if (altTrimmed || descTrimmed) setStatus("Saved (text shortened to fit)", "ok");
        else setStatus("Saved", "ok");
      })
      .catch(function (err) { setStatus("Save failed: " + err, "err"); });
  };

  TBImageDrawer.prototype.insertSelected = function () {
    if (!this.selected) return;
    // Flush any pending debounced save so a user who types and immediately
    // clicks Insert doesn't lose the edit. Inputs also blur naturally when
    // the drawer closes, but the close-then-save race wouldn't reflect the
    // server's trimmed value back into the host page.
    if (this.saveTimer) { clearTimeout(this.saveTimer); this.saveTimer = null; this.saveDetail(); }
    try { this.onInsert(this.selected); } catch (e) { setStatus("Insert failed: " + e, "err"); return; }
    this.close();
  };

  TBImageDrawer.prototype.upload = function (files) {
    if (!files || !files.length) return;
    var self = this;
    var remaining = files.length;
    var failed = 0;
    setStatus("Uploading " + files.length + " file" + (files.length > 1 ? "s" : "") + "…");
    Array.prototype.forEach.call(files, function (file) {
      var fd = new FormData();
      fd.append("file", file);
      fetch("/upload/" + self.slug, { method: "POST", credentials: "same-origin", body: fd })
        .then(function (r) {
          if (!r.ok) throw new Error("HTTP " + r.status);
          return r.json();
        })
        .catch(function () { failed++; })
        .then(function () {
          remaining--;
          if (remaining === 0) {
            if (failed) setStatus(failed + " upload" + (failed > 1 ? "s" : "") + " failed", "err");
            else setStatus("Uploaded", "ok");
            self.fetchAssets();
            var input = el("tb-drawer-upload-input");
            if (input) input.value = "";
          }
        });
    });
  };

  window.TBImageDrawer = new TBImageDrawer();
})();
