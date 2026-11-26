# agent-box website

The marketing and docs site for agent-box, at `agent-box.afig.dev`. Its brand is agent-box's own
(see [BRAND.md](BRAND.md)): dark graphite, amber as the one hot accent, teal as the cool "running"
accent, red reserved for "games win", with Space Grotesk and JetBrains Mono. The hero's signature
element is the throttle meter (My PC / Shared / Full), the switch at the heart of the tool.

It is a plain static site: HTML, one shared stylesheet and SVG art. No build step, no JavaScript,
no backend. Serve the `site/` directory from any static host (Cloudflare Pages, GitHub Pages,
Netlify, or `python3 -m http.server` for a local look).

## Layout

- `index.html` — the landing page: hero + throttle meter, the loop (how it works), the three pieces,
  the three modes, closer. Keeps its own `<style>` block inline.
- `pages.css` — one stylesheet shared by the plain pages below.
- `about/`, `privacy/`, `contact/` — plain pages, each an `index.html` using `pages.css`.
- `llms.txt` — an agent-facing summary and link index.
- `favicon.svg` (the cube mark), `og.svg` (social card), `robots.txt`, `sitemap.xml`.
- `BRAND.md` — the design system. Change a color or a motif there in words before changing the CSS.

## Editing

The plain pages all pull from `pages.css`; the colors and fonts there mirror the tokens the landing
page declares inline, so keep the two palettes in step (or lift them into `pages.css` and `@import`).
Keep the copy in step with the [README](../README.md) and [docs/DESIGN.md](../docs/DESIGN.md): the
site describes the same behavior those do (the three modes, the exit-75 fallback, throwaway boxes,
the tray's per-tick enforcement).

## Local preview

```sh
cd site && python3 -m http.server 8787
```

Then open http://localhost:8787/.
