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

  // Bulk selection. Rows have a checkbox (data-select="row"); group headings
  // and "Select all N shown" toggle the rows in their scope. The #bulk bar
  // shows while anything is ticked, and its buttons send the ticked rows via
  // hx-include. Confirmations are filled in from data-confirm:
  // {count} "3 channels", {units} items to change, {cost} quota units, {left} quota left.
  const rowsIn = (scope) => (scope ? [...scope.querySelectorAll('input[data-select="row"]')] : []);
  const scopeOf = (box) =>
    box.dataset.select === "group" ? box.closest("section") : box.closest("[data-select-scope]");
  const fmt = (n) => n.toLocaleString("en-US");

  function syncSelection() {
    const bar = document.getElementById("bulk");
    if (!bar) return;
    const picked = rowsIn(document).filter((b) => b.checked);
    const n = picked.length;
    const count = `${fmt(n)} ${n === 1 ? bar.dataset.one : bar.dataset.many}`;
    bar.hidden = n === 0;
    document.body.classList.toggle("selecting", n > 0);
    bar.querySelector("[data-bulk-count]").textContent = count;

    for (const box of document.querySelectorAll('input[data-select="group"], input[data-select="all"]')) {
      const rows = rowsIn(scopeOf(box));
      const on = rows.filter((b) => b.checked).length;
      box.checked = on > 0 && on === rows.length;
      box.indeterminate = on > 0 && on < rows.length;
    }

    const left = Number(document.getElementById("summary")?.dataset.quotaLeft ?? 0);
    for (const btn of bar.querySelectorAll("[data-confirm]")) {
      const key = btn.dataset.units; // per-row count to sum, e.g. data-clean
      const units = key ? picked.reduce((sum, b) => sum + Number(b.dataset[key] || 0), 0) : n;
      const cost = units * Number(btn.dataset.cost || 0);
      btn.setAttribute(
        "hx-confirm",
        btn.dataset.confirm
          .replace("{count}", count)
          .replace("{units}", fmt(units))
          .replace("{cost}", fmt(cost))
          .replace("{left}", fmt(left)),
      );
      if (key) btn.disabled = units === 0;
    }
  }

  function clearSelection() {
    for (const row of rowsIn(document)) row.checked = false;
    syncSelection();
  }

  document.addEventListener("change", (e) => {
    const box = e.target;
    if (!(box instanceof HTMLInputElement) || !box.dataset.select) return;
    if (box.dataset.select !== "row") {
      for (const row of rowsIn(scopeOf(box))) row.checked = box.checked;
    }
    syncSelection();
  });
  document.addEventListener("click", (e) => {
    if (e.target instanceof Element && e.target.closest("[data-bulk-clear]")) clearSelection();
  });
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && !document.getElementById("bulk")?.hidden) clearSelection();
  });
  // A bulk action that started (or finished) clears the selection; any swap
  // (filters, a single row re-rendered) recounts it.
  document.addEventListener("htmx:afterRequest", (e) => {
    if (e.detail.successful && e.detail.elt.closest("#bulk")) clearSelection();
  });
  document.addEventListener("htmx:afterSettle", syncSelection);
  document.addEventListener("DOMContentLoaded", syncSelection);
})();
