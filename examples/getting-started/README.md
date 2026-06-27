# kotoji example · Getting started

A small, self-contained, multi-file **static site** that doubles as a
*“How to use kotoji”* guide. It ships in the repo so a fresh operator can
publish it as their very first kotoji site and immediately have a working,
on-box usage guide.

## What's in here

```
getting-started/
├── index.html      # the main usage guide (6 steps + optional extras)
├── features.html   # a second page (demonstrates page-to-page navigation)
├── css/style.css   # self-contained styling (system fonts, dark-friendly, cards)
├── js/app.js       # progressive enhancement (copy buttons, year stamp, active nav)
└── README.md       # this file
```

Relative asset paths used by the pages (all **same-origin**):

- `css/style.css`
- `js/app.js`

No external CDN scripts or fonts are referenced. kotoji serves hosted content
under a strict Content-Security-Policy:

```
default-src 'self'; script-src 'self' 'unsafe-inline' 'unsafe-eval';
style-src 'self' 'unsafe-inline'; img-src 'self' data:; ...
```

so everything this site loads (CSS, JS, the inline-SVG favicon) comes from the
site's own origin. The JS is a progressive enhancement — the pages are fully
usable with JavaScript disabled.

> **Before publishing:** replace every `example.com` placeholder with your
> instance's base domain.

## How to publish it

### Option A — zip upload (no AI needed)

1. Zip the **contents** of this folder so `index.html` sits at the archive root:

   ```bash
   cd examples/getting-started
   zip -r ../getting-started.zip . -x 'README.md'
   ```

   (Excluding `README.md` is optional — it just keeps the published site clean.)

2. In the kotoji dashboard, choose **New**, pick a handle (lowercase letters,
   numbers and hyphens — e.g. `guide`), and select **From a zip**.
3. Upload `getting-started.zip`.
4. Open the **draft** preview, then hit **Publish**. Your guide is now live at
   `guide.example.com`.

### Option B — over MCP (let your AI publish it)

With an MCP client connected (see step 6 in the guide, `<origin>/mcp` + a
`Bearer kotoji_pat_…` token), ask it to:

1. `create_site` with handle `guide` (requires a token that may create sites).
2. `write_file` each file into the `draft` branch:
   `index.html`, `features.html`, `css/style.css`, `js/app.js`.
3. `save` the draft, then `publish` to promote it live.

Each MCP tool takes the `site` handle as a selector, and the token's effective
power on the site is `token.scopes ∩ your-role-on-that-site`.

## Customising

- Edit any file in-browser with the **Monaco** editor — each Save is a commit,
  so you get history, diff and rollback for free.
- Create another **version** (branch) to try a redesign; it gets its own
  per-branch preview URL (`guide--feature-x.example.com`) before you publish.

## License

Part of [kotoji](https://github.com/necorox-com/kotoji), AGPL-3.0 © necorox.
