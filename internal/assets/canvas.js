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

  // outerHTML with our own stamps removed, truncated — this is what the agent
  // gets shown, and it should read as the site's real markup.
  function cleanOuterHTML(el) {
    var s = el.outerHTML.replace(/ data-tb-el="\d+"/g, '');
    return s.length > 4000 ? s.slice(0, 4000) + '…' : s;
  }

  var selected = null;

  function report(el) {
    var msg = { type: 'tb-select', el: null };
    if (el) {
      msg.el = parseInt(el.getAttribute('data-tb-el'), 10);
      msg.tag = el.tagName.toLowerCase();
      msg.breadcrumb = breadcrumb(el);
      msg.outer_html = cleanOuterHTML(el);
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
    var d = e.data || {};
    if (d.type === 'tb-clear') select(null);
  });

  parent.postMessage({ type: 'tb-ready', page: location.pathname }, '*');
})();
