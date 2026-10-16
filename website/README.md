# website

The public landing page for Tasuki: one long page that explains what the platform does, how to integrate it, the
architecture, the scale targets and the roadmap. Plain HTML, CSS and JavaScript with no build step and no
dependencies, so any static host can serve this folder as it is.

| File | What it is |
|---|---|
| `index.html` | The page. Every section, the hero map and the architecture diagram are inline SVG |
| `styles.css` | Light and dark themes (follows the visitor's system setting); code blocks stay dark in both |
| `main.js` | Mobile menu, code tabs, copy buttons, syntax colouring, the hero animation and its sound (off until the visitor turns it on; browsers block audio before a click). The page reads fine without it |
| `favicon.svg` | The mark: an indigo square with the vermilion sash |

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
changes, change its pill in the same pull request. The scale numbers are design targets from
[docs/design.md](../docs/design.md) §4; replace the callout under them with measured results once a load test has run.
The code samples use the real endpoints from [contracts/README.md](../contracts/README.md): update them when the API
changes. Documentation links point at `main` on GitHub.
