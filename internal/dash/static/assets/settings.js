// The Settings page, laid out as Murmur's: sections, a row per setting with a line under it saying
// what it does, saved at once. It holds only what the owner can change here: the theme, which
// lives in this browser. What a host reports about itself (its builds, mode, sizes, caps, expiry
// and GPU) is on that host's page (host.js, #290).

import { h } from "./dom.js";
import { changed } from "./store.js";
import { section } from "./views.js";

export { settings, themeSwitch, row, group };

// The theme, as Murmur's: Dim by default, Dark and Light as options, chosen with a segmented
// radio group. theme.js applied the saved one before the page painted.
const THEMES = [
  ["dark", "Dark"],
  ["dim", "Dim"],
  ["light", "Light"],
];
const THEME_KEY = "agent-box:theme";
function theme() {
  return document.documentElement.dataset.theme || "dim";
}
function setTheme(t) {
  document.documentElement.dataset.theme = t;
  try {
    localStorage.setItem(THEME_KEY, t);
  } catch {}
  changed();
}

function themeSwitch() {
  return h(
    "div",
    { class: "segmented", role: "radiogroup", "aria-label": "Theme" },
    THEMES.map(([id, label]) =>
      h(
        "button",
        {
          role: "radio",
          "aria-checked": theme() === id ? "true" : "false",
          tabindex: theme() === id ? 0 : -1,
          "data-key": "theme-" + id,
          onclick: () => setTheme(id),
          onkeydown: (e) => {
            // One Tab stop; the arrow keys move the choice (WCAG 4.1.2).
            const step = { ArrowRight: 1, ArrowDown: 1, ArrowLeft: -1, ArrowUp: -1 }[e.key];
            if (!step) return;
            e.preventDefault();
            const i = THEMES.findIndex(([t]) => t === theme());
            const next = THEMES[(i + step + THEMES.length) % THEMES.length][0];
            setTheme(next);
            document.querySelector(`[data-key="theme-${next}"]`)?.focus();
          },
        },
        label,
      ),
    ),
  );
}

// A row: its title, one line saying what it is, and its value or control on the right.
function row(title, description, control, feedback) {
  return h(
    "div",
    { class: "row" },
    h("div", { class: "row-grid" }, h("div", null, h("div", { class: "row-title" }, title), description && h("p", { class: "row-description" }, description), feedback), control && h("div", { class: "row-control" }, control)),
  );
}

const group = (rows) => h("div", { class: "group" }, rows);

const settings = {
  mount(el) {
    this.update(el);
  },
  update(el) {
    el.replaceChildren(
      h(
        "div",
        { class: "page" },
        h("div", { class: "page-head" }, h("p", null, "How the dashboard looks. Changes save as you make them; what each host reports about itself is on its page under Fleet.")),
        section("Appearance", null, group([row("Theme", "Dim is the default. Saved in this browser only, so each device keeps its own.", themeSwitch())])),
      ),
    );
  },
};
