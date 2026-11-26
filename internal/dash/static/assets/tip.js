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
// The focused mark, however it was focused, is its chart's one tab stop.
document.addEventListener("focusin", (e) => {
  const m = markOf(e);
  if (!m) return;
  (m.closest("svg, .hbars") || m.parentElement).querySelectorAll("[data-tip]").forEach((o) => o.setAttribute("tabindex", o === m ? "0" : "-1"));
  show(m);
});
document.addEventListener("focusout", (e) => markOf(e) === at && hide());

// Left and right along a row of marks, Home and End to its ends. Up and down go to the nearest
// mark in the row above or below when a chart has rows (<g class="row">), and along the marks
// when it hasn't (the bar lists run down the page).
const centre = (m) => { const r = m.getBoundingClientRect(); return r.left + r.width / 2; };
function nextMark(m, key) {
  const along = (p) => [...p.querySelectorAll(":scope > [data-tip]")];
  const marks = along(m.parentElement);
  const i = marks.indexOf(m);
  if (key === "Home") return marks[0];
  if (key === "End") return marks[marks.length - 1];
  const d = key === "ArrowLeft" || key === "ArrowUp" ? -1 : 1;
  if ((key === "ArrowUp" || key === "ArrowDown") && m.parentElement.matches("g.row")) {
    const rows = [...m.parentElement.parentElement.querySelectorAll(":scope > g.row")].filter((g) => along(g).length);
    const row = rows[rows.indexOf(m.parentElement) + d];
    if (!row) return m;
    const x = centre(m);
    return along(row).reduce((a, b) => (Math.abs(centre(b) - x) < Math.abs(centre(a) - x) ? b : a));
  }
  return marks[Math.min(marks.length - 1, Math.max(0, i + d))];
}
const KEYS = ["ArrowLeft", "ArrowRight", "ArrowUp", "ArrowDown", "Home", "End"];
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") return hide();
  const m = markOf(e);
  if (!m || !KEYS.includes(e.key) || e.altKey || e.ctrlKey || e.metaKey || e.shiftKey) return;
  e.preventDefault();
  const next = nextMark(m, e.key);
  if (next !== m) next.focus(); // focusin makes it the tab stop
});

// A scroll moves the mark: a focused one's tooltip follows it, a hovered one's closes, and the
// tooltip goes back to a mark focused from the keyboard, if there is one.
window.addEventListener(
  "scroll",
  () => {
    if (!at) return;
    if (at === document.activeElement) return place();
    const k = keyed();
    if (k) show(k);
    else hide();
  },
  { capture: true, passive: true },
);
window.addEventListener("resize", hide);
