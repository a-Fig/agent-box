# agent-box brand

agent-box's own identity. Not borrowed from any other project. If something here reads like another
site's look, it's wrong.

## Idea

Raw compute carved out of a gaming PC. The feeling is a machine room, not a chat: dark, technical,
terminal-first, with power you dial up and hand out, and a hard stop the moment a game wants the PC
back. Confident and a little industrial, never cute.

## Color

A dark, blue-free, cream-free system. Amber is the one hot accent (power, heat, throttle); teal is
the cool "running / ready" accent; everything else is graphite.

| Token        | Hex       | Use |
|--------------|-----------|-----|
| `--bg`       | `#0B0E14` | page background, near-black graphite |
| `--panel`    | `#141922` | cards, the terminal, raised surfaces |
| `--panel-2`  | `#1C2230` | hover, insets, the grid lines |
| `--line`     | `#2A3040` | hairlines and borders |
| `--text`     | `#E6EAF2` | primary text |
| `--muted`    | `#9AA4B8` | secondary text |
| `--dim`      | `#5C6477` | tertiary / captions |
| `--amber`    | `#FF7A1A` | primary accent: power, the one loud color |
| `--amber-hi` | `#FFB062` | amber highlight / gradient top |
| `--teal`     | `#39E0C8` | secondary accent: running, ready, success |
| `--red`      | `#FF5D5D` | the only alarm color (paused / games win) |

Amber and teal never appear together as equals; amber leads, teal supports. Red is reserved for the
"games win / paused" state, so it carries meaning.

## Type

- Headings: **Space Grotesk** (700/500) — geometric, machined.
- Mono / labels / terminal: **JetBrains Mono** — the voice of the tool.
- Body: system sans (`ui-sans-serif`), kept quiet so the headings and mono carry the character.

## Logo

An isometric cube drawn as an amber wireframe, with a filled wedge inside showing a fill level: a
box that is partly allocated. It reads as a container and as compute being carved up at once. The
wordmark sets `agent` in `--muted` and `box` in `--text`, or the whole thing in amber on dark.

The mark stands alone as the favicon and OG image. No glass, no mascot, no pastel.

## Motifs

- A faint dot/line grid on the background, like a machine-room floor.
- A horizontal **throttle meter** (My PC / Shared / Full) as the hero's signature element: three
  segments that fill amber as power is handed out, and snap back to red when a game starts.
- Mono-labelled chips and a real terminal block for commands.
- Square or barely-rounded corners (2–4px). This is hardware, not a pillow.
