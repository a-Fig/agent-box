# agent-box website

The marketing and docs site for agent-box, at `agent-box.afig.dev`. It follows Murmur's look
(murmur.afig.dev): the same four colors (blue, cream, butter, ink), Inter, a reeded-glass hero over
blue, and a cream sheet with a rounded top for the pages below it.

It is a plain static site: HTML, one shared stylesheet and an SVG favicon. No build step, no
JavaScript, no backend. Serve the `site/` directory from any static host (Cloudflare Pages, GitHub
Pages, Netlify, or `python3 -m http.server` for a local look).

## Layout

- `index.html` — the landing page: hero, how it works, the three pieces, the three modes.
- `pages.css` — one stylesheet shared by the plain pages below.
- `about/`, `privacy/`, `contact/` — plain pages, each an `index.html` using `pages.css`.
- `llms.txt` — an agent-facing summary and link index.
- `favicon.svg`, `robots.txt`, `sitemap.xml`.

## Editing

The landing page keeps its own `<style>` block inline, because its hero is more than the plain
pages share. The plain pages all pull from `pages.css`; change a color or the footer there once and
every plain page follows. Keep the copy in step with the [README](../README.md) and
[docs/DESIGN.md](../docs/DESIGN.md): the site describes the same behavior those do.

## Local preview

```sh
cd site && python3 -m http.server 8787
```

Then open http://localhost:8787/.
