// Surfaces failed htmx requests as toasts. htmx doesn't swap 4xx/5xx responses,
// so without this a failed Sync or Unsubscribe looks like nothing happened.
(() => {
  const TOAST_MS = 8000;

  function toast(title, detail) {
    const box = document.getElementById("toasts");
    if (!box) return;
    const el = document.createElement("div");
    el.className = "toast";
    el.setAttribute("role", "alert");
    const text = document.createElement("div");
    text.className = "min-w-0 flex-1";
    const t = document.createElement("p");
    t.className = "toast-title";
    t.textContent = title;
    text.append(t);
    if (detail) {
      const d = document.createElement("p");
      d.className = "toast-detail";
      d.textContent = detail;
      text.append(d);
    }
    const close = document.createElement("button");
    close.type = "button";
    close.className = "toast-close";
    close.setAttribute("aria-label", "Dismiss");
    close.textContent = "×";
    close.addEventListener("click", () => el.remove());
    el.append(text, close);
    box.append(el);
    setTimeout(() => el.remove(), TOAST_MS);
  }

  document.addEventListener("htmx:responseError", (e) => {
    const xhr = e.detail.xhr;
    const body = (xhr.responseText || "").trim().slice(0, 400);
    const title = xhr.getResponseHeader("X-Error-Title") || `Request failed (${xhr.status})`;
    toast(title, body);
  });
  document.addEventListener("htmx:sendError", () => {
    toast("Can't reach the dashboard", "Is the server still running?");
  });
})();
