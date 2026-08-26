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

  document.addEventListener('mousemove', function (e) {
    var el = addressable(e.target);
    place(hoverBox, el && el !== selected ? el : null);
  }, true);

  document.addEventListener('mouseleave', function () { place(hoverBox, null); }, true);

  document.addEventListener('click', function (e) {
    e.preventDefault();
    e.stopPropagation();
    var el = addressable(e.target);
    // Clicking the selected element again steps the selection out to its
    // parent — the way to reach a container that's fully covered by children.
    if (el && el === selected) el = addressable(el.parentNode);
    select(el);
  }, true);

  document.addEventListener('submit', function (e) { e.preventDefault(); e.stopPropagation(); }, true);

  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape') select(null);
  }, true);

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
    if (d.type === 'tb-clear') select(null);
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
  });

  parent.postMessage({ type: 'tb-ready', page: location.pathname }, '*');
})();
