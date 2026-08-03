// Micro-interactions that htmx does not cover. Keep this file tiny.

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
