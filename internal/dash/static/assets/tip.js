// Tooltips for chart marks and hints. A mark or a hint carries its text in data-tip ("\n" between
// lines) and is shown in one tooltip of the page's own, on hover, keyboard focus and tap. The
// browser's <title> isn't used: box app's window (a webview) didn't show it, and touch never does
// (#157). Screen readers get the same text from the mark's aria-label or the hint's
// aria-description, so the tooltip itself is aria-hidden.
//
// The tooltip lives on <body>, outside the views. A redraw replaces the elements under it (the
// stream redraws Overview and Boxes every few seconds), so syncTip hands it to the replacement: the
// focused mark, or the one under the pointer that showed it. Escape closes it.
//
// A hint is a control or a word on the page that explains itself (hint). Hints keep the page's
// own tab order. From the keyboard each chart is one tab stop (rove, which marks it data-rove), and
// the arrow keys move between its marks: a day's charts hold over a hundred marks.

export { syncTip, rove, hint, hideTip };

let el = null; // the tooltip, made on first use
let at = null; // the mark it is showing
let point = null; // where the pointer that showed it was, {x, y}; null when focus showed it

function show(mark, p = null) {
  if (!el) {
    el = Object.assign(document.createElement("div"), { className: "tip", hidden: true });
    el.setAttribute("aria-hidden", "true");
    document.body.append(el);
  }
  at = mark;
  point = p;
  el.textContent = mark.dataset.tip;
  el.hidden = false;
  place();
}

function hide() {
  if (!at) return;
  at = null;
  point = null;
  el.hidden = true;
}
const hideTip = hide;

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

// After each redraw: a replaced mark hands its tooltip to the mark now under the pointer that
// showed it, or, if the keyboard's focus showed it, to the focused mark (app.js puts the focus back
// by data-key), with its text read again, so a figure that moved shows its new value. With
// neither, it closes.
function syncTip() {
  if (!at || at.isConnected) return;
  const next = point ? document.elementFromPoint(point.x, point.y)?.closest?.("[data-tip]") : keyed();
  if (next) show(next, point);
  else hide();
}

// One tab stop for a chart's marks: the first.
function rove(chart) {
  chart.setAttribute("data-rove", "");
  chart.querySelectorAll("[data-tip]").forEach((m, i) => m.setAttribute("tabindex", i ? "-1" : "0"));
  return chart;
}

// A hint, added to an element's props: the tooltip's text, which screen readers get as the
// element's description (its name stays its own). A word that does nothing else (term) becomes a
// tab stop, so the keyboard can reach it, and is dotted underneath, so the eye can. Its data-key
// lets a redraw put the focus back on it: term is that key where the same text shows more than
// once on a page (one per box), or true to key it by its text.
function hint(text, props = {}, term = false) {
  const out = { ...props, "data-tip": text, "aria-description": text.replace(/\n/g, " ") };
  if (term) {
    out.tabindex = 0;
    out.class = ((props.class || "") + " hint").trim();
    out["data-key"] = "hint:" + (term === true ? text : term);
  }
  return out;
}

const markOf = (e) => e.target.closest?.("[data-tip]");
const chartOf = (m) => m.closest("[data-rove]");
const keyed = () => document.activeElement?.matches?.("[data-tip]:focus-visible") && document.activeElement;
const tap = (e) => e.pointerType === "touch" || e.pointerType === "pen";

document.addEventListener("pointerover", (e) => {
  if (tap(e)) return; // a tap is handled on pointerdown
  const m = markOf(e);
  if (m) show(m, { x: e.clientX, y: e.clientY });
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
  if (m) show(m, { x: e.clientX, y: e.clientY });
  else hide();
});
// The focused mark, however it was focused, is its chart's one tab stop. Its tooltip shows when
// the keyboard focused it: a click focuses a button too, and a redraw or a closed dialog puts the
// focus back on it, and neither should bring its hint back over the page.
document.addEventListener("focusin", (e) => {
  const m = markOf(e);
  if (!m) return;
  chartOf(m)?.querySelectorAll("[data-tip]").forEach((o) => o.setAttribute("tabindex", o === m ? "0" : "-1"));
  if (at !== m && m.matches(":focus-visible")) show(m);
});
document.addEventListener("focusout", (e) => markOf(e) === at && !point && hide());

// Left and right along a row of marks, Home and End to its ends. Up and down go to the mark in
// the row above or below that covers this one's time when a chart has rows (<g class="row">), and along the marks
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
    // The mark whose span holds this one's centre (gap 0), else the nearest edge: mode segments
    // vary in width, so the nearest centre can be a segment that doesn't cover this time.
    const x = centre(m);
    const gap = (b) => { const r = b.getBoundingClientRect(); return Math.max(r.left - x, 0, x - r.right); };
    return along(row).reduce((a, b) => (gap(b) < gap(a) ? b : a));
  }
  return marks[Math.min(marks.length - 1, Math.max(0, i + d))];
}
const KEYS = ["ArrowLeft", "ArrowRight", "ArrowUp", "ArrowDown", "Home", "End"];
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") return hide();
  const m = markOf(e);
  // Only inside a chart: a hint on a button or a radio leaves its keys alone.
  if (!m || !chartOf(m) || !KEYS.includes(e.key) || e.altKey || e.ctrlKey || e.metaKey || e.shiftKey) return;
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
