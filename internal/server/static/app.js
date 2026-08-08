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
