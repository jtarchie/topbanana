// Canvas edit-mode script, injected into a site served with ?tb_edit=1 (the
// /s/:slug path, same-origin with the admin UI). It turns the page into a
// selection surface: hover outlines the element under the cursor, click
// selects it and reports its address (the server-stamped data-tb-el index) to
// the parent canvas page. Selection is element-identity based — the address,
// never the content — so duplicated text on a page is never ambiguous.
//
// The page's own behavior is suspended while editing: link clicks and form
// submits are swallowed in the capture phase. The overlays are two
// absolutely-positioned boxes; the site's DOM is never mutated beyond them.
(function () {
  'use strict';
  if (window.__tbCanvas) return;
  window.__tbCanvas = true;

  var HALO_Z = 2147483646;

  function makeBox(color, dashed) {
    var b = document.createElement('div');
    b.style.cssText = 'position:absolute;pointer-events:none;z-index:' + HALO_Z +
      ';border:2px ' + (dashed ? 'dashed' : 'solid') + ' ' + color +
      ';border-radius:4px;box-shadow:0 0 0 2px rgba(255,255,255,.65);display:none;';
    document.documentElement.appendChild(b);
    return b;
  }
  var hoverBox = makeBox('#f59e0b', true);
  var selectBox = makeBox('#2563eb', false);

  // dropBar marks where a dragged image will land: a solid line hugging the
  // top or bottom edge of the element under the cursor.
  var dropBar = document.createElement('div');
  dropBar.style.cssText = 'position:absolute;pointer-events:none;z-index:' + HALO_Z +
    ';height:4px;border-radius:2px;background:#2563eb;box-shadow:0 0 0 2px rgba(255,255,255,.65);display:none;';
  document.documentElement.appendChild(dropBar);

  function placeDropBar(el, position) {
    if (!el) { dropBar.style.display = 'none'; return; }
    var r = el.getBoundingClientRect();
    dropBar.style.display = 'block';
    dropBar.style.left = (r.left + window.scrollX) + 'px';
    dropBar.style.width = r.width + 'px';
    dropBar.style.top = ((position === 'before' ? r.top : r.bottom) + window.scrollY - 2) + 'px';
  }

  function place(box, el) {
    if (!el) { box.style.display = 'none'; return; }
    var r = el.getBoundingClientRect();
    box.style.display = 'block';
    box.style.left = (r.left + window.scrollX - 2) + 'px';
    box.style.top = (r.top + window.scrollY - 2) + 'px';
    box.style.width = r.width + 'px';
    box.style.height = r.height + 'px';
  }

  // The addressable ancestor: the nearest element the server stamped. Nodes
  // the browser or page scripts invented have no address; selection climbs to
  // one that does, and the overlays never select themselves.
  function addressable(node) {
    while (node && node !== document.documentElement) {
      if (node === hoverBox || node === selectBox) return null;
      if (node.nodeType === 1 && node.hasAttribute && node.hasAttribute('data-tb-el')) return node;
      node = node.parentNode;
    }
    return null;
  }

  function breadcrumb(el) {
    var parts = [];
    var node = el;
    while (node && parts.length < 4 && node.tagName && node.tagName !== 'HTML') {
      parts.unshift(node.tagName.toLowerCase());
      node = node.parentElement;
    }
    return parts.join(' › ');
  }

  var selected = null;
  // editing holds the in-place text edit in progress:
  // {host, addr, index, original} — host is the stamped element, index the
  // position of the text node among host.childNodes, original its text at
  // edit start (the server's compare-and-set expectation).
  var editing = null;

  // The report carries the element's ADDRESS plus display-only context (tag,
  // breadcrumb, a text snippet for the scope chip). Deliberately no markup:
  // the server resolves the address against the stored source itself, so
  // nothing the framed page's own scripts could forge ever reaches the agent
  // prompt.
  function report(el) {
    var msg = { type: 'tb-select', el: null };
    if (el) {
      msg.el = parseInt(el.getAttribute('data-tb-el'), 10);
      msg.tag = el.tagName.toLowerCase();
      msg.breadcrumb = breadcrumb(el);
      msg.text = (el.textContent || '').trim().slice(0, 120);
    }
    parent.postMessage(msg, '*');
  }

  function select(el) {
    selected = el;
    place(selectBox, el);
    report(el);
  }

  // ----- in-place text editing (double-click a text node) -----

  function caretTextNode(e) {
    var pos = null;
    if (document.caretPositionFromPoint) {
      pos = document.caretPositionFromPoint(e.clientX, e.clientY);
      pos = pos && { node: pos.offsetNode };
    } else if (document.caretRangeFromPoint) {
      var r = document.caretRangeFromPoint(e.clientX, e.clientY);
      pos = r && { node: r.startContainer };
    }
    return pos && pos.node && pos.node.nodeType === 3 ? pos.node : null;
  }

  function startTextEdit(node) {
    var host = node.parentNode;
    // Only direct text children of a stamped element are addressable; text
    // inside script-created markup has no server-side identity.
    if (!host || host.nodeType !== 1 || !host.hasAttribute || !host.hasAttribute('data-tb-el')) return;
    var index = -1, count = 0;
    for (var i = 0; i < host.childNodes.length; i++) {
      if (host.childNodes[i].nodeType === 3) {
        if (host.childNodes[i] === node) index = count;
        count++;
      }
    }
    if (index < 0) return;
    editing = {
      host: host,
      addr: parseInt(host.getAttribute('data-tb-el'), 10),
      index: index,
      original: node.nodeValue
    };
    try { host.contentEditable = 'plaintext-only'; } catch (_) { host.contentEditable = 'true'; }
    host.focus();
    var range = document.createRange();
    range.selectNodeContents(node);
    var sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(range);
    place(selectBox, host);
    place(hoverBox, null);
    parent.postMessage({ type: 'tb-text-start' }, '*');
  }

  function currentTextNode() {
    if (!editing) return null;
    var count = 0;
    for (var i = 0; i < editing.host.childNodes.length; i++) {
      var n = editing.host.childNodes[i];
      if (n.nodeType === 3) {
        if (count === editing.index) return n;
        count++;
      }
    }
    return null;
  }

  function endTextEdit() {
    if (!editing) return;
    editing.host.removeAttribute('contenteditable');
    editing = null;
  }

  function cancelTextEdit() {
    if (!editing) return;
    var node = currentTextNode();
    if (node) node.nodeValue = editing.original;
    endTextEdit();
  }

  function saveTextEdit() {
    if (!editing) return;
    var node = currentTextNode();
    if (!node) {
      // Typing restructured the child nodes (select-all + retype across
      // elements can do this); nothing safe to save deterministically.
      cancelTextEdit();
      parent.postMessage({ type: 'tb-text-note', message: 'That edit was too structural to save in place — use the Describe box.' }, '*');
      return;
    }
    if (node.nodeValue === editing.original) {
      endTextEdit();
      parent.postMessage({ type: 'tb-text-note', message: '' }, '*');
      return;
    }
    parent.postMessage({
      type: 'tb-text-save',
      el: editing.addr,
      text_index: editing.index,
      text: node.nodeValue,
      expect: editing.original
    }, '*');
    // Keep `editing` until the parent reports back, so a failure can revert.
  }

  // ----- pointer + keyboard wiring -----

  // Single-click actions are deferred one beat so a double-click (text edit)
  // doesn't first fire the click path's select/step-out.
  var clickTimer = null;

  document.addEventListener('mousemove', function (e) {
    if (editing) return;
    var el = addressable(e.target);
    place(hoverBox, el && el !== selected ? el : null);
  }, true);

  document.addEventListener('mouseleave', function () { place(hoverBox, null); }, true);

  document.addEventListener('click', function (e) {
    if (editing) {
      // Clicks inside the edited element place the caret; a click anywhere
      // else commits the edit.
      if (editing.host.contains(e.target)) return;
      e.preventDefault();
      e.stopPropagation();
      saveTextEdit();
      return;
    }
    e.preventDefault();
    e.stopPropagation();
    var target = e.target;
    if (clickTimer) clearTimeout(clickTimer);
    clickTimer = setTimeout(function () {
      clickTimer = null;
      var el = addressable(target);
      // Clicking the selected element again steps the selection out to its
      // parent — the way to reach a container that's fully covered by children.
      if (el && el === selected) el = addressable(el.parentNode);
      select(el);
    }, 250);
  }, true);

  document.addEventListener('dblclick', function (e) {
    if (editing) return;
    if (clickTimer) { clearTimeout(clickTimer); clickTimer = null; }
    e.preventDefault();
    e.stopPropagation();
    var node = caretTextNode(e);
    if (node && node.nodeValue.trim() !== '') startTextEdit(node);
  }, true);

  document.addEventListener('submit', function (e) { e.preventDefault(); e.stopPropagation(); }, true);

  // ----- image drop: files dragged from the desktop land as a placement
  // request. The frame can't upload (opaque origin, no credentials); it hands
  // the parent the File plus the target address and edge.
  var dropTarget = null;

  function dragHasFiles(e) {
    var types = e.dataTransfer && e.dataTransfer.types;
    if (!types) return false;
    for (var i = 0; i < types.length; i++) {
      if (types[i] === 'Files') return true;
    }
    return false;
  }

  document.addEventListener('dragover', function (e) {
    if (!dragHasFiles(e)) return;
    e.preventDefault(); // required, or the browser refuses the drop
    var el = addressable(e.target);
    if (el && el.tagName === 'HTML') el = null;
    if (!el) { dropTarget = null; placeDropBar(null); return; }
    var r = el.getBoundingClientRect();
    var position = e.clientY < r.top + r.height / 2 ? 'before' : 'after';
    dropTarget = { el: el, position: position };
    placeDropBar(el, position);
    place(hoverBox, null);
  }, true);

  document.addEventListener('dragleave', function (e) {
    if (!e.relatedTarget) { dropTarget = null; placeDropBar(null); }
  }, true);

  document.addEventListener('drop', function (e) {
    if (!dragHasFiles(e)) return;
    e.preventDefault();
    e.stopPropagation();
    var target = dropTarget;
    dropTarget = null;
    placeDropBar(null);
    var file = e.dataTransfer.files && e.dataTransfer.files[0];
    if (!file || !target) return;
    if (!/^image\//.test(file.type)) {
      parent.postMessage({ type: 'tb-text-note', message: 'Only images can be dropped onto the page.' }, '*');
      return;
    }
    parent.postMessage({
      type: 'tb-image-drop',
      el: parseInt(target.el.getAttribute('data-tb-el'), 10),
      position: target.position,
      file: file
    }, '*');
  }, true);

  document.addEventListener('keydown', function (e) {
    if (editing) {
      if (e.key === 'Enter' && !e.shiftKey) {
        e.preventDefault();
        e.stopPropagation();
        saveTextEdit();
      } else if (e.key === 'Escape') {
        e.preventDefault();
        e.stopPropagation();
        cancelTextEdit();
        parent.postMessage({ type: 'tb-text-note', message: '' }, '*');
      }
      return;
    }
    if (e.key === 'Escape') select(null);
  }, true);

  // ----- scroll reporting: the parent's command bar gets out of the way on
  // scroll-down and returns on scroll-up (the browser-toolbar convention),
  // but the scrolling happens in here — so report direction changes, with
  // hysteresis so a jitter never flaps the bar.
  var lastScrollY = window.scrollY;
  var scrollTick = false;
  window.addEventListener('scroll', function () {
    if (scrollTick) return;
    scrollTick = true;
    requestAnimationFrame(function () {
      scrollTick = false;
      var y = window.scrollY;
      var dy = y - lastScrollY;
      if (Math.abs(dy) < 24) return;
      lastScrollY = y;
      parent.postMessage({ type: 'tb-scroll', dir: dy > 0 ? 'down' : 'up' }, '*');
    });
  }, { passive: true });

  // Keep the halo glued to the element across scroll/resize/layout shifts.
  function refresh() {
    if (selected) place(selectBox, selected);
    requestAnimationFrame(refresh);
  }
  requestAnimationFrame(refresh);

  window.addEventListener('message', function (e) {
    // Only the canvas page may drive the selection — the framed site's own
    // scripts share this window and could otherwise spoof commands.
    if (e.source !== window.parent) return;
    var d = e.data || {};
    // The parent's verdict on a tb-text-save: success releases the edit (the
    // typed text is already on screen and now stored); failure reverts to the
    // original so the page never lies about what's saved.
    if (d.type === 'tb-text-result') {
      if (!editing) return;
      if (d.ok) endTextEdit();
      else cancelTextEdit();
      return;
    }
    if (d.type === 'tb-clear') {
      if (editing) cancelTextEdit();
      select(null);
    }
    // tb-scrollto scrolls the frame programmatically — the automation seam
    // for the scroll-reporting path, since synthetic input can't reach a
    // sandboxed out-of-process frame. Parent-only, like everything here.
    if (d.type === 'tb-scrollto' && typeof d.y === 'number') {
      window.scrollTo(0, d.y);
    }
    // tb-click selects by CSS selector — the canvas's automation seam (tests,
    // and later breadcrumb navigation). Parent-only, and faithful to real
    // click semantics including the step-out-to-parent on reselection, so
    // whatever drives it exercises the same selection rules a pointer does.
    if (d.type === 'tb-click' && typeof d.sel === 'string') {
      var el;
      try { el = document.querySelector(d.sel); } catch (_) { el = null; }
      el = addressable(el);
      if (el && el === selected) el = addressable(el.parentNode);
      select(el);
    }
    // tb-text-edit drives an in-place text edit by selector — the automation
    // seam for the editing path. It joins the production flow at the same
    // point typing does (node mutated, then saveTextEdit), so the save
    // pipeline under test is the real one.
    if (d.type === 'tb-text-edit' && typeof d.sel === 'string') {
      if (editing) return;
      var target;
      try { target = document.querySelector(d.sel); } catch (_) { target = null; }
      var host = addressable(target);
      if (!host) return;
      var want = d.text_index || 0, count = 0, node = null;
      for (var i = 0; i < host.childNodes.length; i++) {
        if (host.childNodes[i].nodeType === 3) {
          if (count === want) { node = host.childNodes[i]; break; }
          count++;
        }
      }
      if (!node) return;
      editing = {
        host: host,
        addr: parseInt(host.getAttribute('data-tb-el'), 10),
        index: want,
        original: node.nodeValue
      };
      node.nodeValue = String(d.text == null ? '' : d.text);
      saveTextEdit();
    }
  });

  parent.postMessage({ type: 'tb-ready', page: location.pathname }, '*');
})();
