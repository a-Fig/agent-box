# Pictures in a pull request

A pull request that changes something a person sees (the dashboard, the Settings page, the tray, the
site under `site/`, terminal output) shows it in its body: a screenshot of the before and the after,
and a GIF when the change moves or responds to input (Tyler, 2026-11-25). A reviewer judges a layout
from a picture faster than from a diff. Both this and `scripts/pr-media.sh` come from Murmur
(a-Fig/murmur, `docs/pr-media.md`).

## Getting a picture into the body

`gh` can't upload images. This repo is public, so `scripts/pr-media.sh` puts them on a `pr-media`
branch and prints Markdown that embeds them by URL:

```sh
scripts/pr-media.sh 247 before.png after.png move.gif   # prints one Markdown image per file
scripts/pr-media.sh --remove 247                        # takes the PR's files off the branch again
```

Paste the printed lines under `## Screenshots` in the body.

- **Nothing in your checkout changes.** It uses only plumbing (`hash-object`, a temporary index,
  `commit-tree`, `push <sha>:refs/heads/pr-media`), so it is safe while someone edits the same
  working tree. The branch holds only pictures.
- **The URLs are pinned to a commit**, so a later push never changes a picture.
- **It is public, and permanent.** History keeps removed files, so `--remove` stops showing a file,
  it doesn't erase it. Check a screenshot for secrets and for anything about the machines first:
  the dashboard's address carries its token, so capture the page, not the address bar; prefer the
  devserver's fixtures to a live host. Keep each file small; the script warns above 5 MB.
- Names are letters, digits, `.`, `_` and `-`; the extension is png, jpg, gif or webp.

## Taking the pictures

**The dashboard.** `go run ./internal/dash/devserver -scenario busy` serves the page with fixture
data on 127.0.0.1:7767 (`docs/ui.md` lists the scenarios), so before and after use the same data.
Take the before from a worktree of the PR's base commit, the after from the branch.

**The site.** `cd site && python3 -m http.server 8787`, then open http://localhost:8787/.

**A page, either one.** `playwright-core` drives a Chromium (`npx playwright install chromium`, or
`executablePath` for one you have):

```js
import { chromium } from 'playwright-core';
const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1100, height: 700 }, deviceScaleFactor: 2 });
await page.goto('http://127.0.0.1:7767/');
await page.screenshot({ path: 'after.png' });       // fullPage: true for a long page
await browser.close();
```

**Something that moves.** Record a video (`recordVideo` on `browser.newContext`, then
`await context.close()` writes it) and turn it into a GIF with a palette:

```sh
ffmpeg -i recording.webm -vf "fps=12,scale=800:-1:flags=lanczos,split[a][b];[a]palettegen[p];[b][p]paletteuse" move.gif
```

**The tray or terminal output.** A screenshot of the tray's menu, or the text in a `<pre>` on a dark
page captured with the code above, so before and after match.
