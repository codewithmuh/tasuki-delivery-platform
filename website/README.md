# website

The public landing page for Tasuki. One long page built around one order followed end to end: as the visitor
scrolls, a pinned scene (the city map, what their system sees, the phone in someone's hand) moves through the five
handoffs, create, dispatch, ride, watch, deliver. Then why a second system, how to integrate, the architecture, the
scale targets, what happens when things break, what is built, how to run it, Japan, the roadmap and the FAQ.
Plain HTML, CSS and JavaScript with no build step and no dependencies, so any static host can serve this folder
as it is.

| File | What it is |
|---|---|
| `index.html` | The page. Every section, both maps and the architecture diagram are inline SVG (the city blocks are drawn by `main.js`) |
| `styles.css` | Light and dark themes (follows the visitor's system setting); code blocks stay dark in both |
| `main.js` | The journey scene (a pure function of scroll position), the hero's moving city, the architecture trace, reveals, counters, the game-day panel, code tabs, copy buttons, syntax colouring, and the sound. The page reads fine without it |
| `favicon.svg` | The mark: an indigo square with the vermilion sash |

## Sound

Every handoff has a sound, synthesised with the Web Audio API from one pentatonic scale (no audio files): a note per
status change, a ping per GPS batch, the offer chime and the decline tone, a rising melody as a packet crosses the
architecture diagram (orders in a low sine voice, GPS points in a brighter triangle voice, with a bass note at
Kafka, a double click at PostgreSQL, a glide at routing), a run as the scale numbers count up, and an alarm then
a resolution in the game-day panel. Sound is on by default with a mute button in the corner, and the choice is
remembered. Browsers refuse to play audio until the visitor clicks, taps or presses a key (scrolling does not
count), so until that first gesture the button pulses and a note says so; the first click on the button starts the
sound rather than muting it. Everything moving respects `prefers-reduced-motion`.

## Preview

```bash
python3 -m http.server 8000 --directory website   # then open http://localhost:8000
```

## Deploy

Point any static host at this folder. There is nothing to build.

| Host | Settings |
|---|---|
| Vercel | Root directory `website`, framework preset "Other", no build command, no output directory |
| Netlify | Base directory `website`, publish directory `website`, no build command |
| Cloudflare Pages | Build output directory `website`, no build command |
| GitHub Pages | A workflow that uploads `website/` with `actions/upload-pages-artifact` and deploys it |

Then put the URL on the repository so it shows in the About box:

```bash
gh repo edit codewithmuh/tasuki-delivery-platform --homepage https://<your-url>
```

## Keeping it honest

The page marks every feature as **Works today**, **In progress**, **Next** or **Planned**. When a feature's state
changes, change its pill in the same pull request. The journey scene, the hero feed and the architecture captions
describe the real flow and the real endpoints from [contracts/README.md](../contracts/README.md): update them when
the API changes (the webhook step is marked as next on the roadmap until it ships). The scale numbers are design
targets from [docs/design.md](../docs/design.md) §4; replace the callout under them with measured results once a
load test has run. Documentation links point at `main` on GitHub.
