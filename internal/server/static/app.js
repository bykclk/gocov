// Micro-interactions that htmx does not cover. Keep this file tiny.

// Banner dismissal: <button data-dismiss="element-id"> hides the element
// for the rest of the browser session (matching the "dismissable per
// session" rule for the open-UI auth banner).
document.querySelectorAll("[data-dismiss]").forEach((btn) => {
  const el = document.getElementById(btn.getAttribute("data-dismiss"));
  if (!el) return;
  const key = "dismissed:" + el.id;
  if (sessionStorage.getItem(key)) el.remove();
  btn.addEventListener("click", () => {
    sessionStorage.setItem(key, "1");
    el.remove();
  });
});

// Destructive-action confirmation: <form data-confirm="message"> asks
// before submitting (e.g. workspace token rotation).
document.addEventListener("submit", (e) => {
  const msg = e.target.getAttribute && e.target.getAttribute("data-confirm");
  if (msg && !window.confirm(msg)) e.preventDefault();
});

// Reveal-a-secret: <button data-reveal="#selector"> swaps a masked value for
// its full form (kept in the target's data-full) and back. Used by the
// onboarding upload-token card so the token is not shown by default.
document.addEventListener("click", (e) => {
  const btn = e.target.closest("[data-reveal]");
  if (!btn) return;
  const el = document.querySelector(btn.getAttribute("data-reveal"));
  if (!el || el.dataset.full === undefined) return;
  if (el.dataset.shown) {
    delete el.dataset.shown;
    el.textContent = el.dataset.masked;
    btn.textContent = "Reveal";
  } else {
    el.dataset.masked = el.textContent;
    el.dataset.shown = "1";
    el.textContent = el.dataset.full;
    btn.textContent = "Hide";
  }
});

// Coverage-gate toggle: the .tg switch enables/disables its paired number
// input. A disabled input is not submitted, so the server reads an empty
// value and treats that gate as off — the same "empty = off" contract the
// settings handler already uses. The header chip tracks how many are on.
document.addEventListener("click", (e) => {
  const tg = e.target.closest(".tg");
  if (!tg) return;
  const gate = tg.closest(".gate");
  const input = gate && gate.querySelector(".gate-input");
  const on = tg.getAttribute("aria-pressed") !== "true";
  tg.setAttribute("aria-pressed", String(on));
  if (on) {
    gate.removeAttribute("data-off");
    if (input) { input.disabled = false; input.focus(); }
  } else {
    gate.setAttribute("data-off", "");
    if (input) input.disabled = true;
  }
  const chip = document.getElementById("gatecount");
  if (chip) {
    const n = document.querySelectorAll(".gate:not([data-off])").length;
    chip.textContent = n === 0 ? "None active" : n + " active";
  }
});

// Settings sidebar: highlight the section link that was clicked.
document.addEventListener("click", (e) => {
  const link = e.target.closest(".side a");
  if (!link) return;
  document.querySelectorAll(".side a").forEach((a) => a.classList.remove("on"));
  link.classList.add("on");
});

// Copy-to-clipboard: <button data-copy="#selector">Copy</button> copies the
// value/text of the referenced element. Falls back to select+execCommand on
// plain-http deployments where the Clipboard API is unavailable.
document.addEventListener("click", (e) => {
  const btn = e.target.closest("[data-copy]");
  if (!btn) return;
  const src = document.querySelector(btn.getAttribute("data-copy"));
  if (!src) return;
  const text = src.value !== undefined ? src.value : src.textContent;

  const done = () => {
    const old = btn.textContent;
    btn.textContent = "Copied!";
    btn.disabled = true;
    setTimeout(() => { btn.textContent = old; btn.disabled = false; }, 1200);
  };

  const legacyCopy = () => {
    if (!src.select) return;
    src.select();
    document.execCommand("copy");
    done();
  };
  if (navigator.clipboard && window.isSecureContext) {
    navigator.clipboard.writeText(text).then(done, legacyCopy);
  } else {
    legacyCopy();
  }
});

// Files view: Tree/List toggle, filter tabs (All/Changed/Source/Coverage), search, and tree collapsible directories.
(function () {
  function getRows(table) {
    return table ? Array.prototype.slice.call(table.querySelectorAll('tbody tr')) : [];
  }

  function applyFileFilters() {
    var card = document.getElementById('files-card');
    if (!card) return;
    var searchInput = document.getElementById('file-search');
    var matchCount = document.getElementById('file-matches');
    var emptyMsg = document.getElementById('files-empty');
    var activeFilter = card.dataset.filter || 'all';

    var q = (searchInput ? searchInput.value : '').trim().toLowerCase();
    var f = activeFilter;
    var listTable = document.getElementById('filetable');
    var treeTable = document.getElementById('filetable-tree');

    // Filter List table
    var listRows = getRows(listTable);
    var matchedFiles = 0;
    listRows.forEach(function (tr) {
      var p = (tr.dataset.path || '').toLowerCase();
      var okSearch = !q || p.indexOf(q) >= 0;
      var okFilter = f === 'all' ||
        (f === 'changed' && tr.dataset.changed === 'true') ||
        (f === 'source' && tr.dataset.source === 'true') ||
        (f === 'coverage' && tr.dataset.coverage === 'true');
      var show = okSearch && okFilter;
      tr.style.display = show ? '' : 'none';
      if (show) matchedFiles++;
    });

    // Filter Tree table
    if (treeTable) {
      var treeRows = getRows(treeTable);
      var visiblePaths = {};
      treeRows.forEach(function (tr) {
        if (tr.classList.contains('tree-file')) {
          var p = (tr.dataset.path || '').toLowerCase();
          var okSearch = !q || p.indexOf(q) >= 0;
          var okFilter = f === 'all' ||
            (f === 'changed' && tr.dataset.changed === 'true') ||
            (f === 'source' && tr.dataset.source === 'true') ||
            (f === 'coverage' && tr.dataset.coverage === 'true');
          if (okSearch && okFilter) {
            visiblePaths[tr.dataset.path] = true;
            var parts = (tr.dataset.path || '').split('/');
            var acc = '';
            for (var i = 0; i < parts.length - 1; i++) {
              acc = acc ? acc + '/' + parts[i] : parts[i];
              visiblePaths[acc] = true;
            }
          }
        }
      });

      var collapsedPrefixes = [];
      treeRows.forEach(function (tr) {
        var path = tr.dataset.path || '';
        var isDir = tr.classList.contains('tree-dir');
        var shouldShow = visiblePaths[path] === true;

        // A search reaches into collapsed directories; the fold only
        // applies while browsing.
        var isCollapsed = false;
        for (var i = 0; !q && i < collapsedPrefixes.length; i++) {
          if (path.indexOf(collapsedPrefixes[i] + '/') === 0) {
            isCollapsed = true;
            break;
          }
        }

        if (isDir) {
          var toggle = tr.querySelector('.tree-toggle');
          if (toggle && toggle.getAttribute('aria-expanded') === 'false') {
            collapsedPrefixes.push(path);
          }
        }

        tr.style.display = (shouldShow && !isCollapsed) ? '' : 'none';
      });
    }

    if (matchCount) {
      matchCount.textContent = matchedFiles + (matchedFiles === 1 ? ' file' : ' files');
    }
    if (emptyMsg) {
      emptyMsg.hidden = matchedFiles > 0;
    }
  }

  // View mode: Tree vs List. The choice is remembered per browser so a
  // reader who prefers the flat list is not switched back on every page.
  var viewKey = 'gocov.filesView';
  function setView(view, remember) {
    var card = document.getElementById('files-card');
    var seg = document.getElementById('view-mode');
    if (!card || !seg) return;
    card.dataset.view = view;
    seg.querySelectorAll('button').forEach(function (b) {
      b.setAttribute('aria-pressed', String(b.dataset.view === view));
    });
    if (remember) {
      try { localStorage.setItem(viewKey, view); } catch (err) { /* storage unavailable */ }
    }
  }
  document.addEventListener('click', function (e) {
    var btn = e.target.closest('#view-mode button');
    if (!btn) return;
    setView(btn.dataset.view, true);
  });

  // Filter switcher: All / Changed / Source / Coverage
  document.addEventListener('click', function (e) {
    var btn = e.target.closest('#file-filters button');
    if (!btn) return;
    var seg = btn.closest('#file-filters');
    var card = document.getElementById('files-card');
    if (!card) return;
    var f = btn.dataset.filter;
    card.dataset.filter = f;
    seg.querySelectorAll('button').forEach(function (b) {
      b.setAttribute('aria-pressed', String(b.dataset.filter === f));
    });
    applyFileFilters();
  });

  // Search input
  document.addEventListener('input', function (e) {
    if (e.target && e.target.id === 'file-search') {
      applyFileFilters();
    }
  });

  // Tree directory expand/collapse
  document.addEventListener('click', function (e) {
    var btn = e.target.closest('.tree-toggle');
    if (!btn) return;
    var expanded = btn.getAttribute('aria-expanded') === 'true';
    btn.setAttribute('aria-expanded', String(!expanded));
    btn.innerHTML = expanded ? '&#9656;' : '&#9662;';
    applyFileFilters();
  });

  // Run on initial page load and after every htmx swap
  function init() {
    var card = document.getElementById('files-card');
    if (!card) return;
    var stored = null;
    try { stored = localStorage.getItem(viewKey); } catch (err) { /* storage unavailable */ }
    if ((stored === 'tree' || stored === 'list') && stored !== card.dataset.view) {
      setView(stored, false);
    }
    if (card.dataset.filter && card.dataset.filter !== 'all') {
      applyFileFilters();
    }
  }
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
  document.addEventListener('htmx:afterSwap', function () {
    init();
  });
})();

  // Analytics: the layout renders <meta name="gocov-posthog"> only when the
// operator configured a PostHog key (GOCOV_POSTHOG_KEY); without it nothing
// below runs and no third-party script is loaded. The setup is deliberately
// narrow — no cookie or localStorage (memory persistence), no autocapture,
// Do Not Track honoured — and signed-in users are identified by their
// numeric gocov id, never by email. Session replay runs only on the sign-in
// and setup pages (replayPaths), with every input masked and the upload
// token card blocked (class ph-no-capture); report, upload and source pages
// are never recorded.
(function () {
  const meta = document.querySelector('meta[name="gocov-posthog"]');
  if (!meta || !meta.content) return;
  // The pages worth watching someone go through, and nothing that renders
  // coverage or source. The PostHog project carries the same list as a URL
  // trigger, so a page outside it is never recorded even if this drifts.
  const replayPaths = /^\/(login|register|onboarding|github\/setup|workspaces\/[^/]+\/setup)\/?$/;
  const replay = replayPaths.test(location.pathname);
  const script = document.createElement("script");
  script.src = meta.dataset.host + "/static/array.js";
  script.async = true;
  script.onload = () => {
    if (!window.posthog) return;
    posthog.init(meta.content, {
      api_host: meta.dataset.host,
      persistence: "memory",
      // The app shows private repo names and source; no click text, no
      // heatmaps. JS errors are fine: message and stack only.
      autocapture: false,
      enable_heatmaps: false,
      capture_exceptions: true,
      disable_session_recording: !replay,
      session_recording: { maskAllInputs: true, blockClass: "ph-no-capture" },
      capture_pageview: false,
      // Page leaves give PostHog bounce rate and time on page; with memory
      // persistence they still stay within the one page load.
      capture_pageleave: true,
      // Core Web Vitals (LCP, INP, CLS): performance numbers, no content.
      capture_performance: { web_vitals: true },
      respect_dnt: true,
      person_profiles: "identified_only",
    });
    if (meta.dataset.user) posthog.identify("user:" + meta.dataset.user);
    const pageview = () => {
      // ?ref= is how the landing page and badges attribute a visit; PostHog
      // only picks up utm_* on its own, so carry it explicitly.
      const ref = new URLSearchParams(location.search).get("ref");
      posthog.capture("$pageview", ref ? { ref } : undefined);
    };
    pageview();
    // htmx swaps that push a URL (branch switch, upload paging) are
    // navigations to the reader even though the document never reloads.
    document.addEventListener("htmx:pushedIntoHistory", pageview);

    // Product events are declared in the templates, next to the UI they
    // describe: data-ph-view="<event>" fires once when the page loads,
    // data-ph-click="<event>" on click. Every event carries the wizard
    // context from the element that declares data-ph-step (step, face,
    // forge) plus the declaring element's own data-ph-* values, so a click
    // knows which page and forge it happened on. Event names only — never
    // form values.
    const reserved = ["phView", "phClick"];
    const context = (el) => {
      const props = {};
      const root = document.querySelector("[data-ph-step]");
      for (const node of root && root !== el ? [root, el] : [el]) {
        for (const [k, v] of Object.entries(node.dataset)) {
          if (k.startsWith("ph") && !reserved.includes(k)) props[k.slice(2).toLowerCase()] = v;
        }
      }
      return props;
    };
    document.querySelectorAll("[data-ph-view]").forEach((el) => posthog.capture(el.dataset.phView, context(el)));
    document.addEventListener("click", (e) => {
      const el = e.target.closest("[data-ph-click]");
      if (el) posthog.capture(el.dataset.phClick, context(el));
    });
  };
  document.head.appendChild(script);
})();
