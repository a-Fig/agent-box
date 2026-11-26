# agent-box website

The marketing and docs site for agent-box, at `agent-box.afig.dev`. It lives in the same warm,
crafted family as the house look (blue + cream, soft light, Inter, one butter accent), with its own
hero and signature element — see [BRAND.md](BRAND.md).

It is a plain static site: HTML, one shared stylesheet and SVG art. No build step, no JavaScript,
no backend. Serve the `site/` directory from any static host (Cloudflare Pages, GitHub Pages,
Netlify, or `python3 -m http.server` for a local look).

## Layout

- `index.html` — the landing page: a hero with a soft blue→butter glow and a live **status card**
  (the PC's mode and the boxes running on it, agent-box's signature element), then a stat strip, a
  three-step "one command" flow, a parts grid, and a mode table. Keeps its own `<style>` inline.
- `pages.css` — one stylesheet shared by the plain pages below.
- `about/`, `privacy/`, `contact/` — plain pages, each an `index.html` using `pages.css`.
- `llms.txt` — an agent-facing summary and link index.
- `favicon.svg` (the cube mark), `og.svg` (social card), `robots.txt`, `sitemap.xml`.
- `BRAND.md` — the design system, and what keeps it from being a copy of the house look.

## Editing

The plain pages pull from `pages.css`; its colors mirror the tokens the landing page declares
inline, so keep the two palettes in step. Keep the copy in step with the [README](../README.md)
and [docs/DESIGN.md](../docs/DESIGN.md): the site describes the same behavior (the three modes, the
exit-75 fallback, throwaway boxes, the tray's per-tick enforcement).

## Local preview

```sh
cd site && python3 -m http.server 8787
```

Then open http://localhost:8787/.
