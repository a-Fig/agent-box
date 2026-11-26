// Tooltips for chart marks. A mark carries its text in data-tip ("\n" between lines) and is shown
// in one tooltip of the page's own, on hover, keyboard focus and tap. The browser's <title> isn't
// used: box app's window (a webview) didn't show it, and touch never does (#157). Screen readers
// get the same text from the mark's aria-label, so the tooltip itself is aria-hidden.
//
// The tooltip lives on <body>, outside the views, so a redraw that keeps the mark keeps it open;
// syncTip closes it when its mark is gone, and a scroll or Escape closes it too.

export { syncTip };

let el = null; // the tooltip, made on first use
let at = null; // the mark it is showing

function show(mark) {
  if (!el) {
    el = Object.assign(document.createElement("div"), { className: "tip", hidden: true });
    el.setAttribute("aria-hidden", "true");
    document.body.append(el);
  }
  at = mark;
  el.textContent = mark.dataset.tip;
  el.hidden = false;
  place();
}

function hide() {
  if (!at) return;
  at = null;
  el.hidden = true;
}

// Above the mark and centred on it, kept inside the window; under it when there is no room above.
function place() {
  const r = at.getBoundingClientRect();
  const w = el.offsetWidth, ht = el.offsetHeight, gap = 6, pad = 8;
  const x = Math.min(Math.max(pad, r.left + r.width / 2 - w / 2), window.innerWidth - w - pad);
  const y = r.top - ht - gap >= pad ? r.top - ht - gap : Math.min(r.bottom + gap, window.innerHeight - ht - pad);
  el.style.left = Math.round(x) + "px";
  el.style.top = Math.round(y) + "px";
}

// After each redraw: a mark that was replaced takes its tooltip with it.
function syncTip() {
  if (at && !at.isConnected) hide();
}

const markOf = (e) => e.target.closest?.("[data-tip]");

document.addEventListener("pointerover", (e) => {
  if (e.pointerType === "touch") return; // a tap is handled on pointerdown
  const m = markOf(e);
  if (m) show(m);
});
document.addEventListener("pointerout", (e) => {
  if (e.pointerType === "touch" || !at) return;
  // A mark focused from the keyboard keeps its tooltip; one clicked with the mouse doesn't.
  if (markOf(e) === at && !at.contains(e.relatedTarget) && !at.matches(":focus-visible")) hide();
});
// A tap on a mark shows it until a tap elsewhere.
document.addEventListener("pointerdown", (e) => {
  if (e.pointerType !== "touch") return;
  const m = markOf(e);
  if (m) show(m);
  else hide();
});
document.addEventListener("focusin", (e) => {
  const m = markOf(e);
  if (m) show(m);
});
document.addEventListener("focusout", (e) => markOf(e) === at && hide());
document.addEventListener("keydown", (e) => e.key === "Escape" && hide());
window.addEventListener("scroll", hide, { capture: true, passive: true });
window.addEventListener("resize", hide);
