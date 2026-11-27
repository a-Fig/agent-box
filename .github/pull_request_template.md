<!-- Title: type(scope): what changes, said as the behavior a user or agent sees.
  type: fix, feat, docs, test, refactor, perf or ci.
  scope: the part it touches, such as box, mcp, boxd, dash, tray, mode, provision, site, docs or ci. Join several with a comma: feat(box,mcp).
  Two real titles, the style we copy (from Murmur's template, after T3 Code's):
    fix(box): a throwaway box is deleted only after its job's exit code is read
    feat(dash): a box opens to show the jobs it ran -->

Closes #
<!-- Use Closes #N when this PR to main completes an issue. Remove the line otherwise. -->

## Preview
<!-- Only for a PR that changes the website (site/). Delete this section, heading and all, when it doesn't.
  The preview URL, on its own line. Until it's up, write "Preview link incoming." and replace it with the link once it is. -->

## Why

## Change

## Screenshots
<!-- If this changes something a person sees (the dashboard or Settings page, the tray, the site, terminal output): before and after screenshots, plus a GIF when it moves or responds to input. Paste the Markdown that `scripts/pr-media.sh <pr> <files>` prints (docs/pr-media.md). Otherwise: "None: nothing visible changes." -->

## Agent-facing text
<!-- If this changes text an agent reads (MCP tool descriptions or results, the box skill, box help or error output): the text as an agent receives it, pasted from a real run and not retyped, in a code block. For a change to existing text, before and after. Put each sample in its own `<details>` block, with a summary line that names it. Otherwise: "None: no agent-facing text changes." -->

## Verified
<!-- What ran and what was seen. Provisioning and tray changes: what ran on the real PC. What could not be checked. -->

## Review
<!-- Which commits the Opus, Fable or Sol review covered. Docs-only PRs: say so. -->
