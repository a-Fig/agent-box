// Tooltips for chart marks. A mark carries its text in data-tip ("\n" between lines) and is shown
// in one tooltip of the page's own, on hover, keyboard focus and tap. The browser's <title> isn't
// used: box app's window (a webview) didn't show it, and touch never does (#157). Screen readers
// get the same text from the mark's aria-label, so the tooltip itself is aria-hidden.
//
// The tooltip lives on <body>, outside the views, so a redraw that keeps the mark keeps it open;
// syncTip closes it when its mark is gone, and Escape closes it too.
//
// From the keyboard each chart is one tab stop (rove), and the arrow keys move between its marks:
// a day's charts hold over a hundred marks.

export { syncTip, rove };

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
// Measured at 0,0 first: a box left near the right edge would measure narrow.
function place() {
  el.style.left = el.style.top = "0px";
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

// One tab stop for a chart's marks: the first.
function rove(chart) {
  chart.querySelectorAll("[data-tip]").forEach((m, i) => m.setAttribute("tabindex", i ? "-1" : "0"));
  return chart;
}

const markOf = (e) => e.target.closest?.("[data-tip]");
const keyed = () => document.activeElement?.matches?.("[data-tip]:focus-visible") && document.activeElement;
const tap = (e) => e.pointerType === "touch" || e.pointerType === "pen";

document.addEventListener("pointerover", (e) => {
  if (tap(e)) return; // a tap is handled on pointerdown
  const m = markOf(e);
  if (m) show(m);
});
document.addEventListener("pointerout", (e) => {
  if (tap(e) || !at || markOf(e) !== at || at.contains(e.relatedTarget)) return;
  // Back to the mark focused from the keyboard, if there is one; a clicked mark lets go.
  const k = keyed();
  if (k) show(k);
  else hide();
});
// A tap on a mark shows it until a tap elsewhere.
document.addEventListener("pointerdown", (e) => {
  if (!tap(e)) return;
  const m = markOf(e);
  if (m) show(m);
  else hide();
});
document.addEventListener("focusin", (e) => {
  const m = markOf(e);
  if (m) show(m);
});
document.addEventListener("focusout", (e) => markOf(e) === at && hide());
const STEP = { ArrowLeft: -1, ArrowUp: -1, ArrowRight: 1, ArrowDown: 1 };
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") return hide();
  const m = markOf(e);
  if (!m || !(e.key in STEP || e.key === "Home" || e.key === "End")) return;
  const marks = [...m.parentElement.querySelectorAll(":scope > [data-tip]")];
  const i = marks.indexOf(m);
  const next = marks[e.key === "Home" ? 0 : e.key === "End" ? marks.length - 1 : Math.min(marks.length - 1, Math.max(0, i + (STEP[e.key] || 0)))];
  e.preventDefault();
  if (next === m) return;
  m.setAttribute("tabindex", "-1");
  next.setAttribute("tabindex", "0");
  next.focus();
});
// A scroll moves the mark: a focused one's tooltip follows it, a hovered one's closes.
window.addEventListener("scroll", () => at && (at === document.activeElement ? place() : hide()), { capture: true, passive: true });
window.addEventListener("resize", hide);
