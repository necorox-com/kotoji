# kotoji — Atomic Design System & Frontend UI Guidelines

> **Status:** Locked design, implementation-ready.
> **Scope:** This document is the single source of truth for the kotoji **frontend** (Next.js 16 App Router + React 19 + TypeScript strict). It defines the brand, design tokens, the full atomic component inventory, the page wireframes, and the data/auth/theming/a11y layer. Backend (Go) and API contract specifics are referenced where the UI depends on them but defined elsewhere.
>
> **Audience:** Anyone implementing or reviewing kotoji UI. Read top-to-bottom once; thereafter use as a lookup.

---

## 0. How to read this document

- **§1 Principles** — the *why*; the feel we are protecting.
- **§2 Tokens** — the *constants*; copy these verbatim into `globals.css` / `@theme`.
- **§3 Atomic hierarchy** — the *components*; every screen is built only from things listed here.
- **§4 Data / auth / theming / a11y / conventions** — the *plumbing*; how a component actually fetches, guards, animates, and where its file lives.
- **§5 Open questions / gaps** — unresolved decisions surfaced deliberately (考慮漏れ).

Everything concrete (hex, px, signatures, folder paths) is intended to be pasted, not paraphrased.

---

## 1. Design principles

kotoji is a *home* for tools that non-engineers and their AI build. The UI's job is to make hosting feel **calm, safe, and reversible** — never like a deploy console.

### 1.1 Brand feel — "the bridge that supports the strings" (琴柱)

The name comes from the **kotoji-tōrō** lantern in Kenrokuen, Kanazawa, and from *kotoji* (琴柱), the small bridge that holds up the strings of a koto. The product *supports* the user's tools; it does not compete with them. Translated to UI:

- **kotoji is the frame, the user's tool is the content.** Chrome (nav, panels, badges) is quiet and recedes; the user's served page / their code in Monaco is the loud, colorful thing. Never let our UI out-shout the content it hosts.
- **Kanazawa flavor, applied with restraint.** A single brand accent drawn from **加賀の藍 (Kaga indigo)** — a deep, slightly grayed blue — plus warm off-white "washi paper" backgrounds and gold-leaf (金箔) used *only* as a rare success/celebration glint (e.g. the moment a publish succeeds). No literal lanterns, no torii clip-art. The mark is a stylized koto-bridge "人"-shaped glyph; the palette and typography carry the flavor.
- **Wabi over flash.** Generous whitespace, hairline borders, soft shadows. Motion is short and eases out; nothing bounces.

### 1.2 Core principles

1. **Clarity for non-engineers first.** Plain-language labels ("公開する" / "Publish", "下書き" / "Draft"), never raw git/jargon in the default path. GitHub, SHA, branch internals are progressively disclosed (an "Advanced" / 上級 affordance), never required.
2. **Safety & reversibility are visible.** Because git underlies everything, *nothing is destructive*. The UI states this: "前のバージョンに戻せます / You can always roll back." Publish is a deliberate, distinct, confirmed action — separated from Save in color, placement, and copy.
3. **One mental model: Draft → (review) → Published.** Every screen reinforces this pipeline. Branch previews are framed as "別バージョン / alternate versions," not as a VCS feature.
4. **Trustworthy = honest state.** Loading, empty, error, and stale (optimistic-lock conflict) states are designed, not afterthoughts. We never show a fake success.
5. **Bilingual-ready (ja default, en).** Layout must survive both. JP text wraps differently and runs longer in places; never hard-code widths to fit one language. (i18n library choice is a gap — see §5; design assumes string keys, not literals.)
6. **Mobile-first, but honest about the editor.** The dashboard, lists, publish, and history are first-class on phones. The Monaco code editor is a *desktop/tablet* experience; on phones we offer a read-friendly, "request edit / open on bigger screen" fallback rather than a cramped editor. (See §3 templates.)
7. **Accessible by default (WCAG AA).** Keyboard-operable, visible focus, sufficient contrast, respects `prefers-reduced-motion`. A11y is a token-and-primitive decision, made once here.

---

## 2. Design tokens

Tokens are the only source of visual constants. Components reference **semantic** tokens (e.g. `bg-card`, `text-muted-foreground`, `ring-ring`), never raw hex or raw ramp steps. Tailwind v4 is configured CSS-first via `@theme` + CSS custom properties; `next-themes` flips a `.dark` class on `<html>`.

### 2.1 Color system

#### 2.1.1 Approach

- We define a **neutral gray ramp** (warm, "washi"-leaning, not pure cold gray), a **brand ramp** (加賀藍 Kaga indigo), and **status ramps** (success/warning/info/destructive). These are the *primitive* scales.
- We then map **semantic tokens** (background, foreground, card, …) onto those primitives, separately for light and dark.
- All values are authored in OKLCH in the actual CSS for perceptual consistency (Tailwind v4 / shadcn default), but **suggested hex equivalents are given below** so designers and reviewers have concrete reference values. Hex are approximate sRGB renderings of the intended OKLCH; treat OKLCH as canonical when both are present.
- **Contrast contract:** every `*-foreground` on its paired surface must meet **WCAG AA** — ≥ 4.5:1 for body text, ≥ 3:1 for large text (≥ 24px or ≥ 18.66px bold) and for UI component boundaries. The values below are chosen to satisfy this; re-verify if you retune.

#### 2.1.2 Primitive ramps (hex reference)

**Neutral — "washi" warm gray** (very slight warm/indigo tint so the UI reads as paper, not steel):

| Step | Hex | Typical use |
|---|---|---|
| `neutral-0`  | `#ffffff` | pure white (rare; cards in dark, highlights) |
| `neutral-50` | `#faf9f7` | app background (light) |
| `neutral-100`| `#f3f1ee` | muted surfaces, hover (light) |
| `neutral-200`| `#e7e4df` | borders, dividers (light) |
| `neutral-300`| `#d6d2cb` | input borders, disabled fills |
| `neutral-400`| `#a8a39a` | placeholder text, disabled text |
| `neutral-500`| `#7c766c` | muted foreground (light) |
| `neutral-600`| `#5b554c` | secondary text |
| `neutral-700`| `#3f3a33` | body text on light (high contrast) |
| `neutral-800`| `#2a2620` | headings (light) / card (dark) |
| `neutral-850`| `#211d18` | elevated surface (dark) |
| `neutral-900`| `#19160f` | app background (dark) |
| `neutral-950`| `#0f0d09` | deepest (dark, behind cards) |

**Brand — 加賀藍 (Kaga indigo)** primary:

| Step | Hex | Note |
|---|---|---|
| `indigo-50`  | `#eef2fb` | tint backgrounds, selected rows (light) |
| `indigo-100` | `#dbe3f6` | hover tint |
| `indigo-200` | `#b9c6ec` | |
| `indigo-300` | `#8ea3dd` | |
| `indigo-400` | `#5e74c4` | brand accent on dark surfaces |
| `indigo-500` | `#3a52a8` | **primary (light)** — deep indigo |
| `indigo-600` | `#2f4490` | primary hover (light) |
| `indigo-700` | `#263872` | primary active / pressed |
| `indigo-800` | `#1d2b57` | |
| `indigo-900` | `#16213f` | |

**Gold leaf — 金箔 (accent, used sparingly)** — a warm gold for celebratory success glints and the brand mark, NOT a general accent:

| Step | Hex | Note |
|---|---|---|
| `gold-300` | `#e6c66a` | publish-success glint, mark highlight |
| `gold-400` | `#d4af37` | classic gold leaf |
| `gold-500` | `#b8932a` | text-safe gold on light (use carefully) |

**Status ramps** (each: a mid for fills, a dark for text-on-light, a tint for backgrounds):

| Role | tint (bg) | mid (fill) | text-on-light | text-on-dark | dark-tint |
|---|---|---|---|---|---|
| success (緑) | `#e9f6ee` | `#2e9e5b` | `#1f7a44` | `#5cd98a` | `#10301f` |
| warning (琥珀) | `#fbf1e0` | `#d98a1f` | `#9a6310` | `#f0b860` | `#36260d` |
| info (空) | `#e8f2fb` | `#2f7fc4` | `#1f5e96` | `#6fb4e8` | `#0e2740` |
| destructive (加賀の紅 lacquer-red) | `#fbeceb` | `#c0392b` | `#a02b20` | `#ef8278` | `#3a1310` |

> **Why indigo primary + lacquer-red destructive:** Kanazawa's Kaga craft palette is built on 藍 (indigo) and 紅/朱 (lacquer red). Using indigo for trust/primary and reserving the red specifically for *destructive* keeps the danger signal unambiguous while staying on-brand. Gold is the celebration note, never a routine button.

#### 2.1.3 Semantic tokens — Light theme

Defined on `:root`. (Hex shown; author as OKLCH in CSS.)

| Token | Hex | Maps to | Notes |
|---|---|---|---|
| `--background` | `#faf9f7` | neutral-50 | app canvas |
| `--foreground` | `#2a2620` | neutral-800 | body text, AA on background |
| `--card` | `#ffffff` | neutral-0 | raised cards/panels |
| `--card-foreground` | `#2a2620` | neutral-800 | |
| `--popover` | `#ffffff` | neutral-0 | menus, tooltips bg |
| `--popover-foreground` | `#2a2620` | neutral-800 | |
| `--primary` | `#3a52a8` | indigo-500 | primary buttons, active nav |
| `--primary-foreground` | `#ffffff` | neutral-0 | text on primary (AA: ~6.4:1) |
| `--secondary` | `#f3f1ee` | neutral-100 | secondary buttons, chips |
| `--secondary-foreground` | `#3f3a33` | neutral-700 | |
| `--muted` | `#f3f1ee` | neutral-100 | muted surfaces |
| `--muted-foreground` | `#7c766c` | neutral-500 | secondary/help text, AA large+ on muted |
| `--accent` | `#eef2fb` | indigo-50 | hover/selected tint, focus-row |
| `--accent-foreground` | `#263872` | indigo-700 | |
| `--destructive` | `#c0392b` | red-mid | delete buttons |
| `--destructive-foreground` | `#ffffff` | neutral-0 | |
| `--success` | `#2e9e5b` | success-mid | published badge fill |
| `--success-foreground` | `#ffffff` | | |
| `--warning` | `#d98a1f` | warning-mid | conflict / stale badge |
| `--warning-foreground` | `#2a2620` | neutral-800 | dark text on amber for AA |
| `--info` | `#2f7fc4` | info-mid | informational |
| `--info-foreground` | `#ffffff` | | |
| `--border` | `#e7e4df` | neutral-200 | hairline borders, dividers |
| `--input` | `#d6d2cb` | neutral-300 | input/control border |
| `--ring` | `#5e74c4` | indigo-400 | focus ring (3px, see a11y) |
| `--brand-gold` | `#d4af37` | gold-400 | mark + success glint only |

#### 2.1.4 Semantic tokens — Dark theme

Defined on `.dark`. Surfaces step *up* in lightness with elevation (background darkest, card lighter).

| Token | Hex | Maps to | Notes |
|---|---|---|---|
| `--background` | `#19160f` | neutral-900 | warm near-black (not pure) |
| `--foreground` | `#ece8e1` | ~neutral-100 inv | AA on background |
| `--card` | `#211d18` | neutral-850 | |
| `--card-foreground` | `#ece8e1` | | |
| `--popover` | `#2a2620` | neutral-800 | |
| `--popover-foreground` | `#ece8e1` | | |
| `--primary` | `#7e93d6` | ~indigo-350 | lighter indigo for contrast on dark |
| `--primary-foreground` | `#12182b` | indigo-900-ish | dark text on light-indigo button (AA) |
| `--secondary` | `#2a2620` | neutral-800 | |
| `--secondary-foreground` | `#ece8e1` | | |
| `--muted` | `#211d18` | neutral-850 | |
| `--muted-foreground` | `#a8a39a` | neutral-400 | AA large on muted |
| `--accent` | `#26304f` | indigo-deep tint | selected row |
| `--accent-foreground` | `#cdd7f3` | indigo-150 | |
| `--destructive` | `#ef8278` | red-on-dark | lighter red, AA text-as-button uses dark fg |
| `--destructive-foreground` | `#3a1310` | red dark-tint | |
| `--success` | `#5cd98a` | success-on-dark | |
| `--success-foreground` | `#10301f` | | |
| `--warning` | `#f0b860` | warning-on-dark | |
| `--warning-foreground` | `#36260d` | | |
| `--info` | `#6fb4e8` | info-on-dark | |
| `--info-foreground` | `#0e2740` | | |
| `--border` | `#332e26` | neutral ~750 | |
| `--input` | `#3f3a33` | neutral-700 | |
| `--ring` | `#7e93d6` | indigo-350 | |
| `--brand-gold` | `#e6c66a` | gold-300 | |

> **Dark destructive note:** on dark, a solid lighter-red fill with dark text reads better than a dark-red fill with white text. The token pairing above is for the **filled** variant; the **outline/ghost** destructive variant uses `--destructive` as text/border on `--background`.

#### 2.1.5 Chart / editor sync tokens (optional but reserved)

- `--editor-bg`, `--editor-fg`, `--editor-line`, `--editor-selection` — derived from the theme so Monaco's theme can be generated to match (see §4.6). In light: editor bg = `--card` (`#ffffff`), in dark: `#1b1813` (slightly off `--card` to separate the code surface). Monaco gets a custom theme `kotoji-light` / `kotoji-dark` built from these.

### 2.2 `@theme` / CSS — copy-paste skeleton

```css
/* src/app/globals.css */
@import "tailwindcss";
@import "tw-animate-css";

@custom-variant dark (&:is(.dark *));

:root {
  /* radius */
  --radius: 0.625rem; /* 10px base; see radius scale */

  /* semantic — light */
  --background: oklch(0.985 0.004 80);      /* #faf9f7 */
  --foreground: oklch(0.27 0.012 70);       /* #2a2620 */
  --card: oklch(1 0 0);                      /* #ffffff */
  --card-foreground: oklch(0.27 0.012 70);
  --popover: oklch(1 0 0);
  --popover-foreground: oklch(0.27 0.012 70);
  --primary: oklch(0.45 0.11 268);           /* #3a52a8 Kaga indigo */
  --primary-foreground: oklch(1 0 0);
  --secondary: oklch(0.955 0.005 80);        /* #f3f1ee */
  --secondary-foreground: oklch(0.34 0.012 70);
  --muted: oklch(0.955 0.005 80);
  --muted-foreground: oklch(0.55 0.012 70);  /* #7c766c */
  --accent: oklch(0.955 0.02 268);           /* #eef2fb indigo-50 */
  --accent-foreground: oklch(0.34 0.09 268);
  --destructive: oklch(0.55 0.18 27);        /* lacquer red */
  --destructive-foreground: oklch(1 0 0);
  --success: oklch(0.62 0.14 152);
  --success-foreground: oklch(1 0 0);
  --warning: oklch(0.72 0.14 70);
  --warning-foreground: oklch(0.27 0.012 70);
  --info: oklch(0.6 0.12 240);
  --info-foreground: oklch(1 0 0);
  --border: oklch(0.91 0.005 80);            /* #e7e4df */
  --input: oklch(0.85 0.006 80);             /* #d6d2cb */
  --ring: oklch(0.55 0.1 268);               /* indigo-400 */
  --brand-gold: oklch(0.78 0.12 90);         /* #d4af37 */

  --editor-bg: var(--card);
  --editor-fg: var(--foreground);
}

.dark {
  --background: oklch(0.17 0.008 75);        /* #19160f */
  --foreground: oklch(0.92 0.006 80);
  --card: oklch(0.21 0.008 75);
  --card-foreground: oklch(0.92 0.006 80);
  --popover: oklch(0.26 0.008 75);
  --popover-foreground: oklch(0.92 0.006 80);
  --primary: oklch(0.66 0.1 268);            /* lighter indigo */
  --primary-foreground: oklch(0.2 0.05 268);
  --secondary: oklch(0.26 0.008 75);
  --secondary-foreground: oklch(0.92 0.006 80);
  --muted: oklch(0.21 0.008 75);
  --muted-foreground: oklch(0.68 0.01 75);
  --accent: oklch(0.32 0.05 268);
  --accent-foreground: oklch(0.85 0.06 268);
  --destructive: oklch(0.7 0.14 25);
  --destructive-foreground: oklch(0.22 0.06 25);
  --success: oklch(0.78 0.14 152);
  --success-foreground: oklch(0.22 0.06 152);
  --warning: oklch(0.8 0.13 75);
  --warning-foreground: oklch(0.24 0.05 75);
  --info: oklch(0.74 0.11 240);
  --info-foreground: oklch(0.2 0.05 240);
  --border: oklch(0.3 0.006 75);
  --input: oklch(0.36 0.008 75);
  --ring: oklch(0.66 0.1 268);
  --brand-gold: oklch(0.82 0.1 90);

  --editor-bg: oklch(0.165 0.01 70);
  --editor-fg: oklch(0.92 0.006 80);
}

@theme inline {
  /* expose semantic tokens to Tailwind utilities: bg-background, text-foreground, etc. */
  --color-background: var(--background);
  --color-foreground: var(--foreground);
  --color-card: var(--card);
  --color-card-foreground: var(--card-foreground);
  --color-popover: var(--popover);
  --color-popover-foreground: var(--popover-foreground);
  --color-primary: var(--primary);
  --color-primary-foreground: var(--primary-foreground);
  --color-secondary: var(--secondary);
  --color-secondary-foreground: var(--secondary-foreground);
  --color-muted: var(--muted);
  --color-muted-foreground: var(--muted-foreground);
  --color-accent: var(--accent);
  --color-accent-foreground: var(--accent-foreground);
  --color-destructive: var(--destructive);
  --color-destructive-foreground: var(--destructive-foreground);
  --color-success: var(--success);
  --color-success-foreground: var(--success-foreground);
  --color-warning: var(--warning);
  --color-warning-foreground: var(--warning-foreground);
  --color-info: var(--info);
  --color-info-foreground: var(--info-foreground);
  --color-border: var(--border);
  --color-input: var(--input);
  --color-ring: var(--ring);
  --color-brand-gold: var(--brand-gold);

  /* radius scale */
  --radius-sm: calc(var(--radius) - 4px);
  --radius-md: calc(var(--radius) - 2px);
  --radius-lg: var(--radius);
  --radius-xl: calc(var(--radius) + 4px);

  /* fonts (see typography) */
  --font-sans: var(--font-inter), "Noto Sans JP", system-ui, sans-serif;
  --font-mono: var(--font-jbmono), "Source Han Code JP", ui-monospace, monospace;

  /* breakpoints (see breakpoints section — these are Tailwind defaults, restated) */
  --breakpoint-sm: 40rem;   /* 640 */
  --breakpoint-md: 48rem;   /* 768 */
  --breakpoint-lg: 64rem;   /* 1024 */
  --breakpoint-xl: 80rem;   /* 1280 */
  --breakpoint-2xl: 96rem;  /* 1536 */
}

@layer base {
  * { @apply border-border outline-ring/50; }
  body { @apply bg-background text-foreground font-sans antialiased; }
}
```

### 2.3 Typography

**Font stack (JP-capable):**

- **Sans (UI):** `Inter` (latin) + `Noto Sans JP` (japanese) → `--font-sans`. Loaded via `next/font/google` with `display: "swap"` and **subset/preload** disabled for Noto Sans JP (it's huge; load `weight: ["400","500","700"]` only, `preload:false`, `adjustFontFallback` on). System fallback `system-ui` → Hiragino/Yu Gothic on JP machines.
- **Mono (code, SHAs, URLs):** `JetBrains Mono` + `Source Han Code JP` (or `BIZ UDGothic` fallback) → `--font-mono`. Monaco loads its own font separately (see §4.6) but mono UI text uses this.

> **Performance note:** Noto Sans JP at full weight set is multi-MB. Restrict to 3 weights, `preload:false`, rely on `font-display: swap`, and let `system-ui` paint first. Do NOT self-host the full CJK file unsubset.

**Type scale** (rem; 1rem = 16px). Line-heights tuned for JP (CJK needs a bit more leading than latin):

| Token | size | line-height | weight default | Tailwind | Use |
|---|---|---|---|---|---|
| `text-display` | 2.25rem / 36px | 2.75rem / 44px | 700 | `text-4xl` | marketing/empty hero only |
| `text-h1` | 1.875rem / 30px | 2.375rem / 38px | 700 | `text-3xl` | page title |
| `text-h2` | 1.5rem / 24px | 2rem / 32px | 600 | `text-2xl` | section title |
| `text-h3` | 1.25rem / 20px | 1.75rem / 28px | 600 | `text-xl` | card title, panel header |
| `text-h4` | 1.125rem / 18px | 1.625rem / 26px | 600 | `text-lg` | sub-section |
| `text-body` | 1rem / 16px | 1.625rem / 26px | 400 | `text-base` | default body (JP-friendly LH 1.6) |
| `text-sm` | 0.875rem / 14px | 1.375rem / 22px | 400 | `text-sm` | secondary, table cells |
| `text-xs` | 0.75rem / 12px | 1.125rem / 18px | 500 | `text-xs` | badges, captions, labels |
| `text-code` | 0.8125rem / 13px | 1.375rem / 22px | 400 | `font-mono text-[13px]` | inline code, SHA, URL |

- **Weights used:** 400 (body), 500 (labels/badges/emphasis), 600 (headings), 700 (page/display). No light/300 (poor JP rendering at small sizes).
- **Letter-spacing:** default 0; headings ≥ h2 use `-0.01em` (latin only effect). Never apply negative tracking to JP-heavy strings.
- **Truncation:** handles, file paths, URLs truncate with `…` + tooltip showing full value. Never wrap a handle/URL mid-token in a card.

### 2.4 Spacing scale

Tailwind's default 4px base (`spacing` unit = 0.25rem). We standardize the *meaningful* steps and their roles:

| Step | px | Role |
|---|---|---|
| `0.5` | 2 | hairline gaps, icon nudge |
| `1` | 4 | tight inline gap (icon↔label) |
| `2` | 8 | control inner padding, chip gap |
| `3` | 12 | compact card padding, form row gap |
| `4` | 16 | **default gap** between elements |
| `5` | 20 | |
| `6` | 24 | card padding, section inner gap |
| `8` | 32 | section vertical rhythm |
| `10` | 40 | |
| `12` | 48 | page top padding (desktop) |
| `16` | 64 | large empty-state vertical |

- **Page gutters:** mobile `px-4` (16), tablet `px-6` (24), desktop `px-8` (32).
- **Vertical rhythm between major sections:** `space-y-8` desktop, `space-y-6` mobile.
- **Touch targets:** every interactive atom ≥ **44×44px** effective hit area on touch (pad small icon buttons accordingly).

### 2.5 Radius

`--radius` base = **10px** (`0.625rem`). Soft but not pill — calm, paper-like.

| Token | px | Use |
|---|---|---|
| `rounded-sm` | 6 | badges, kbd, small chips |
| `rounded-md` | 8 | inputs, buttons, menu items |
| `rounded-lg` | 10 | cards, panels, dialogs |
| `rounded-xl` | 14 | large surfaces, hero, dropzone |
| `rounded-full` | ∞ | avatars, spinner, status dots |

### 2.6 Shadows / elevation

Soft, low-spread, warm-tinted (not pure black) to match washi feel. Dark theme uses near-black with higher opacity + a subtle inset top hairline for separation.

| Token | Light value | Use |
|---|---|---|
| `shadow-xs` | `0 1px 2px oklch(0.27 0.01 70 / 0.06)` | inputs, badges resting |
| `shadow-sm` | `0 1px 3px oklch(0.27 0.01 70 / 0.08), 0 1px 2px oklch(0.27 0.01 70 / 0.04)` | cards |
| `shadow-md` | `0 4px 12px oklch(0.27 0.01 70 / 0.10)` | popover, dropdown, hover-lift |
| `shadow-lg` | `0 12px 32px oklch(0.27 0.01 70 / 0.14)` | dialog, sheet |
| `shadow-focus` | `0 0 0 3px var(--ring) / 0.45` | (use ring utilities, not box-shadow, in practice) |

Dark: replace base color with `oklch(0 0 0 / …)` at ~2× opacity, and add `inset 0 1px 0 oklch(1 0 0 / 0.04)` on cards/popovers for an edge highlight.

> **Elevation philosophy:** kotoji prefers **borders over shadows** to delineate (calmer). Shadows appear only on *floating* layers (popover, dropdown, dialog, sheet, drag) and a subtle hover-lift on interactive cards.

### 2.7 z-index layers

Centralized so nothing collides. Expose as Tailwind utilities via `@theme` (`--z-*`) or use Radix portal defaults; this is the authority:

| Layer | z | Members |
|---|---|---|
| base | 0 | normal flow |
| sticky | 10 | sticky table headers, branch bar |
| nav | 20 | top nav, sidebar |
| dropdown | 30 | menus, selects, comboboxes, tooltips |
| overlay | 40 | dialog/sheet backdrop |
| modal | 50 | dialog, sheet, command palette |
| toast | 60 | sonner toaster |
| max | 9999 | dev banners, debug |

(Radix portals render to `body`; we keep their internal z within these bands. Toaster mounted last/highest.)

### 2.8 Breakpoints

Tailwind v4 defaults, mapped to kotoji's phone/tablet/desktop language:

| Class prefix | min-width | kotoji label | Layout intent |
|---|---|---|---|
| (base) | 0 | **phone** (<640) | single column, drawers, stacked, no Monaco edit |
| `sm:` | 640px | phone-landscape / small tablet | start showing 2-up cards |
| `md:` | 768px | **tablet** (640–1024 band, esp. ≥768) | sidebar can appear; split-pane collapses to tabs |
| `lg:` | 1024px | **desktop** | full split-pane (tree + editor + side panel), persistent sidebar |
| `xl:` | 1280px | wide desktop | wider editor, optional 3-pane comfortable |
| `2xl:` | 1536px | ultra-wide | max container, more whitespace |

**The three canonical bands (per requirements):** phone `< 640` (base) · tablet `640–1024` (`sm`/`md`) · desktop `> 1024` (`lg`+). Design and QA against **375px (phone), 768px (tablet), 1280px (desktop)** as the reference widths.

### 2.9 Container widths

| Context | max-width | notes |
|---|---|---|
| App content (dashboard, lists, forms) | `max-w-screen-xl` (1280) centered, with gutters | |
| Reading/marketing/empty hero | `max-w-2xl` (672) | |
| Auth card | `max-w-sm` (384) | |
| ProjectDetail | **full-bleed** (no max) | split-pane needs all width |
| Forms (CreateSite, Settings) | `max-w-2xl` (672) | single column |
| Dialog | `sm:max-w-md` (448) default; `lg` for diff preview | |

### 2.10 Iconography & motion tokens (summary; detail in §4)

- **Icons:** `lucide-react`, stroke 1.75, sizes 16 (inline), 18 (buttons), 20 (nav), 24 (empty-state). Color `currentColor`. Decorative icons `aria-hidden`; meaningful icons get `aria-label`.
- **Motion durations:** `--motion-fast: 120ms`, `--motion-base: 180ms`, `--motion-slow: 260ms`; easing `cubic-bezier(0.2, 0, 0, 1)` (ease-out). All gated by `prefers-reduced-motion` (see §4.7).

---

## 3. Atomic hierarchy & component inventory

Methodology: **Atoms → Molecules → Organisms → Templates → Pages.** Each entry lists: the kotoji component name, its underlying shadcn/Radix primitive(s), key props/variants, and kotoji-specific behavior. shadcn-generated primitives live in `src/components/ui/`; kotoji wrappers live in `atoms/molecules/organisms/templates` (see §4.9 conventions).

### 3.0 Component map at a glance

```
ATOMS      Button Input Textarea Label StatusBadge Icon Spinner Switch
           Checkbox RadioGroup Avatar Kbd Tooltip Separator Skeleton
           Badge Chip Link CodeText Progress VisuallyHidden

MOLECULES  FormField SearchBar BranchSelect FileTreeItem ProjectCard
           CopyableUrl ConfirmDialog EmptyState Toast(sonner) FileTypeIcon
           CommitItem MemberRow RoleSelect PublishStatusPill DiffStat
           ThemeToggle UserMenu Pagination FileBreadcrumb TabBar
           UploadDropzoneTrigger MetaStat InlineAlert

ORGANISMS  TopNav AppSidebar ProjectGrid FileTree MonacoEditorPanel
           DiffViewer BranchBar PublishPanel HistoryTimeline UploadDropzone
           MemberTable CreateSiteForm SiteSettingsForm CommandPalette
           ConflictResolver MobileFileDrawer EditorToolbar

TEMPLATES  AuthLayout DashboardLayout ProjectDetailLayout(split-pane) AdminLayout

PAGES      Login Dashboard CreateSite ProjectDetail
           (tabs: Files/Editor · Branches · Publish · History · Members · Settings)
           Admin (Users · Projects · Quotas · ReservedWords)
```

---

### 3.1 Atoms

The smallest indivisible UI units. No business logic, no data fetching, no app state. Pure presentational + a11y.

| Atom | shadcn/Radix base | Variants / props | kotoji specifics |
|---|---|---|---|
| **Button** | shadcn `button` (Radix Slot, CVA) | `variant`: `default`(primary indigo) / `secondary` / `outline` / `ghost` / `destructive` / `link` / **`publish`**(gold-tinted, reserved); `size`: `sm`/`default`/`lg`/`icon`; `loading?` (shows Spinner, disables, keeps width); `asChild` | `publish` variant exists *only* for the publish CTA so it's visually unique. Loading state announces via `aria-busy`. Min height 36 (sm 32, lg 40); icon button 36×36 padded to 44 hit area. |
| **Input** | shadcn `input` | `invalid?`, `size`, type | invalid → `aria-invalid`, `border-destructive`, `ring-destructive`. Mono variant for handle/SHA fields (`font-mono`). |
| **Textarea** | shadcn `textarea` | auto-resize optional | for commit messages, descriptions. |
| **Label** | shadcn `label` (Radix Label) | `required?` | renders `*` with `aria-hidden` + sr text "必須". Always tied via `htmlFor`. |
| **StatusBadge** | shadcn `badge` + CVA | `status`: `published`/`draft`/`preview`/`building`/`error`/`stale`/`offline` | semantic color map: published→success, draft→neutral, preview→info, building→info+pulse, error→destructive, stale(conflict)→warning. Always icon + text (never color alone — a11y). |
| **Badge** | shadcn `badge` | generic count/label badge | for file counts, member counts. |
| **Chip** | `badge` variant `outline` | removable? (x button) | branch chips, filter chips. |
| **Icon** | `lucide-react` | size 16/18/20/24 | wrapper enforcing stroke + a11y (decorative `aria-hidden`). |
| **Spinner** | custom (lucide `Loader2` + `animate-spin`) | size, `label` | reduced-motion → swap spin for opacity pulse. `role="status"` + sr label. |
| **Progress** | shadcn `progress` (Radix Progress) | determinate/indeterminate | upload progress, publish steps. |
| **Switch** | shadcn `switch` (Radix Switch) | | theme toggle internals, settings toggles. AA focus ring. |
| **Checkbox** | shadcn `checkbox` (Radix) | | member bulk select, settings. |
| **RadioGroup** | shadcn `radio-group` (Radix) | | CreateSite mode (empty/zip/template). |
| **Avatar** | shadcn `avatar` (Radix Avatar) | size, fallback initials | user menu, member rows; fallback = initials on indigo tint. |
| **Kbd** | custom `<kbd>` | | shortcut hints (⌘K, ⌘S, Esc). Mono, `rounded-sm`, border. |
| **Tooltip** | shadcn `tooltip` (Radix Tooltip) | side, delay | for truncated text, icon-only buttons. Provider mounted once at root. |
| **Separator** | shadcn `separator` (Radix) | orientation | hairline `--border`. |
| **Skeleton** | shadcn `skeleton` | | loading placeholders mirroring final layout. reduced-motion safe. |
| **Link** | Next `<Link>` wrapper | `variant`: nav/inline | inline = underline on hover, indigo; visited not differentiated (app context). |
| **CodeText** | custom `<code>` | | inline mono for handle/SHA/path; selectable; `rounded-sm bg-muted px-1`. |
| **VisuallyHidden** | Radix `VisuallyHidden` or `sr-only` | | a11y labels. |

**Button variant CVA (canonical):**

```tsx
// src/components/ui/button.tsx (shadcn-generated, extended)
const buttonVariants = cva(
  "inline-flex items-center justify-center gap-2 whitespace-nowrap rounded-md text-sm font-medium " +
  "transition-colors focus-visible:outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50 " +
  "disabled:pointer-events-none disabled:opacity-50 [&_svg]:size-4 [&_svg]:shrink-0 aria-busy:cursor-progress",
  {
    variants: {
      variant: {
        default:     "bg-primary text-primary-foreground shadow-xs hover:bg-primary/90",
        secondary:   "bg-secondary text-secondary-foreground hover:bg-secondary/80",
        outline:     "border border-input bg-background hover:bg-accent hover:text-accent-foreground",
        ghost:       "hover:bg-accent hover:text-accent-foreground",
        link:        "text-primary underline-offset-4 hover:underline",
        destructive: "bg-destructive text-destructive-foreground shadow-xs hover:bg-destructive/90",
        publish:     "bg-primary text-primary-foreground shadow-sm ring-1 ring-inset ring-brand-gold/40 hover:bg-primary/90",
      },
      size: {
        sm: "h-8 px-3 text-xs", default: "h-9 px-4", lg: "h-10 px-6 text-base", icon: "size-9",
      },
    },
    defaultVariants: { variant: "default", size: "default" },
  },
);
```

---

### 3.2 Molecules

Atoms composed into a single reusable interaction unit. May hold *local* UI state (open/closed, copied), but **no server fetching** — they take data via props and emit events.

| Molecule | Composed of | Purpose / behavior |
|---|---|---|
| **FormField** | Label + Input/Textarea/Select + help text + error | wraps `react-hook-form` field; renders `aria-describedby` for help+error, `aria-invalid`. The single way forms render fields. |
| **SearchBar** | Input + Icon(Search) + clear button + Kbd(/) | filters project/file lists; debounced (250ms) onChange; `/` focuses it. |
| **BranchSelect** | shadcn `select` or `command` combobox + StatusBadge | switch active branch; shows published/draft/feature-* with status dot + preview-URL affordance; "新しいバージョンを作成 / New version" action at bottom. Non-engineer copy hides "branch" → "バージョン". |
| **FileTreeItem** | Icon(FileTypeIcon/chevron) + name + optional StatusBadge | one row of the FileTree; indent by depth; selected/hover states; keyboard focusable; right-aligned dirty-dot when unsaved. |
| **FileTypeIcon** | lucide map | `.html`→FileCode, `.css`→Palette, `.js`→FileJson2, img→Image, folder→Folder/FolderOpen. |
| **ProjectCard** | Card + StatusBadge + CopyableUrl + MetaStat + thumbnail | dashboard tile: handle (title), description, published status, last-updated relative time, copyable URL, hover-lift, whole card is a link to detail. |
| **CopyableUrl** | CodeText + Button(icon Copy) + Tooltip + Toast | shows `handle.hosting.example.com`; click copies, toast "コピーしました", icon → Check 1.5s. Truncates middle, full on tooltip. |
| **ConfirmDialog** | shadcn `alert-dialog` (Radix) | reusable confirm for destructive/irreversible-ish actions (delete project, rollback, publish). Props: title, description, confirmLabel, `variant`(destructive/default), typed-confirmation? (for delete, require typing handle). |
| **EmptyState** | Icon(24) + heading + body + primary action | no projects / no files / no history / no members. Friendly JP copy + illustration slot (koto-bridge line glyph). |
| **Toast** | `sonner` | success/error/info/loading→success transitions. Mounted via `<Toaster richColors closeButton />`. Used for save/publish/copy/errors. |
| **CommitItem** | Avatar + message + CodeText(short SHA) + relative time + source-tag | one history entry; source-tag chip = upload/editor/MCP/github; click → diff. |
| **DiffStat** | two CodeText (+adds/−dels) | `+12 −3` colored success/destructive; shown on commits & publish panel. |
| **MemberRow** | Avatar + name/email + RoleSelect + remove Button | one row of MemberTable. |
| **RoleSelect** | shadcn `select` | owner/editor/viewer (roles per §5 gap). disabled if only-owner. |
| **PublishStatusPill** | StatusBadge + relative time | "公開中 · 2時間前" / "下書きが先行 (未公開の変更あり)". The at-a-glance publish state. |
| **ThemeToggle** | Button(icon) + dropdown (light/dark/system) | uses next-themes; icons Sun/Moon/Monitor; mounted in TopNav + mobile menu. Hydration-safe (mounted guard). |
| **UserMenu** | Avatar + shadcn `dropdown-menu` | profile, theme, admin link (if admin), sign out (→ backend logout). |
| **Pagination** | Buttons + page indicator | history list / admin tables. |
| **FileBreadcrumb** | Links + chevrons | current file path in editor header; segments truncate. |
| **TabBar** | shadcn `tabs` (Radix Tabs) | ProjectDetail section tabs (Files/Branches/Publish/History/Members/Settings); on desktop may be sidebar instead (see template). |
| **UploadDropzoneTrigger** | Button + hidden file input | the click-to-pick part used inside UploadDropzone and CreateSite. |
| **MetaStat** | Icon + label + value | "最終更新 2h", "ファイル 14", "メンバー 3". |
| **InlineAlert** | shadcn `alert` (Radix-less) | inline contextual message: stale-conflict warning, publish-blocked reason, info banners. `variant`: info/warning/destructive/success. |
| **SourceTag** | Chip + Icon | provenance of a change: `Upload`/`Editor`/`MCP`(AI)/`GitHub`. Color-coded subtly; AI/MCP gets a distinct sparkle icon so AI-authored changes are legible. |

---

### 3.3 Organisms

Composed, app-aware sections. These **may** consume hooks (TanStack Query) and dispatch mutations, but UI logic stays here and data logic lives in the hooks (§4). Each is independently testable with a mocked query client.

| Organism | Composed of | Behavior & responsive notes |
|---|---|---|
| **TopNav** | logo/mark (koto-bridge glyph) + breadcrumb + SearchBar(global, ⌘K) + ThemeToggle + UserMenu | sticky `z-nav`. On phone: collapses to logo + hamburger (opens AppSidebar as a Sheet) + UserMenu. Hosts the CommandPalette trigger. |
| **AppSidebar** | nav links (Dashboard, Admin*), recent projects, "New site" CTA | desktop (`lg:`): persistent left rail (collapsible to icons). tablet/phone: Radix `Sheet` drawer from TopNav hamburger. Active route highlighted (`accent`). |
| **ProjectGrid** | grid of ProjectCard + SearchBar + filter Chips + EmptyState + Skeletons | responsive grid: 1col phone, 2col `sm`, 3col `lg`, 4col `2xl`. Filters: status, mine/all. Loading → skeleton cards; empty → EmptyState with "最初のサイトを作る". |
| **FileTree** | recursive FileTreeItem + context menu (rename/delete/new) | left pane of editor. Keyboard nav (↑↓ move, → expand, ← collapse, Enter open). Selected file drives MonacoEditorPanel. On phone → not inline; opened via **MobileFileDrawer**. Virtualized if large. |
| **MonacoEditorPanel** | `@monaco-editor/react` + EditorToolbar + status footer | center pane. Loads file content (lazy `dynamic(..., {ssr:false})`). Tracks dirty state, exposes Save (⌘S). Theme synced (kotoji-light/dark). Footer: language, ln/col, base-SHA indicator, dirty dot. **Phone:** replaced by read-only viewer + "大きな画面で編集 / Open on larger screen" + "AIに編集を依頼" hint (per principle #6). |
| **EditorToolbar** | FileBreadcrumb + Save Button + branch chip + "差分を見る/View diff" + overflow menu | sticky top of editor pane. Save shows base-SHA optimistic state; on conflict shows ConflictResolver. |
| **DiffViewer** | Monaco `DiffEditor` (or `react-diff`) + header (from→to) + DiffStat | side-by-side on desktop, inline/unified on phone (`renderSideBySide={isDesktop}`). Used in History (commit diff) and pre-publish (draft↔published). Read-only. |
| **BranchBar** | BranchSelect + per-branch CopyableUrl(preview) + "公開" Button + PublishStatusPill | sticky `z-sticky` strip above editor / on Branches tab. Shows current branch, its preview URL, and quick publish. |
| **PublishPanel** | DiffViewer(draft↔published summary) + commit message + Publish Button(`publish` variant) + ConfirmDialog + result Toast | the Publish tab/flow. Shows *what will change*, requires confirm, runs mutation with progress, celebratory gold glint + toast on success. For non-engineers shows "公開をリクエスト / Request publish" when GitHub-PR delegation is on (per spec). |
| **HistoryTimeline** | vertical list of CommitItem grouped by date + Pagination + filter(branch/source) | git log view. Click commit → DiffViewer; each has "このバージョンに戻す / Roll back" → ConfirmDialog → rollback mutation. SourceTag shows upload/editor/MCP/github provenance. |
| **UploadDropzone** | drag-area + UploadDropzoneTrigger + Progress + file-validation feedback | zip upload. Client-side pre-check: extension `.zip`, soft size warning; server enforces ZipSlip/bomb/allowlist (UI surfaces server rejection clearly via InlineAlert). Used in CreateSite and "re-upload" in detail. |
| **MemberTable** | table of MemberRow + invite form + EmptyState | Members tab. Desktop = table; phone = stacked cards. Owner-only mutations. |
| **CreateSiteForm** | RadioGroup(empty/zip/template) + FormField(handle, with live validation) + conditional UploadDropzone/template picker + submit | handle validation inline (lowercase a-z0-9-, length, reserved-word, uniqueness async-debounced); shows resulting URL preview live: `{handle}.hosting.example.com`. |
| **SiteSettingsForm** | FormField(rename handle → shows redirect note) + GitHub link section + danger zone(delete via ConfirmDialog typed) | Settings tab. Advanced/GitHub disclosed under a collapsible. |
| **ConflictResolver** | InlineAlert(warning) + DiffViewer(yours↔server) + actions(reload/overwrite) | shown when Save returns base-SHA mismatch (optimistic lock). Explains in plain language; default safe action = reload server version; overwrite is secondary + confirmed. |
| **CommandPalette** | shadcn `command` (cmdk) in Dialog (⌘K) | jump to project, switch branch, run actions (new site, publish, toggle theme). Power-user; fully keyboard. |
| **MobileFileDrawer** | Sheet + FileTree | phone-only: file tree in a bottom/left sheet; selecting closes drawer and shows file. |
| **AdminUserTable / AdminProjectTable / QuotaPanel / ReservedWordsEditor** | tables + forms | Admin organisms (see Admin page). |

---

### 3.4 Templates (layouts)

Templates define the *skeleton* and responsive behavior; pages slot content in. Implemented with Next App Router nested layouts where natural.

#### 3.4.1 `AuthLayout`
- Centered single card (`max-w-sm`) on `--background`, brand mark + tagline above. No nav. Used by Login.
- Responsive: identical across breakpoints; card stays centered, full-width-minus-gutter on phone.

#### 3.4.2 `DashboardLayout`
- **Desktop (`lg:`):** persistent `AppSidebar` (left) + `TopNav` (top) + scrollable content (`max-w-screen-xl`, gutters).
- **Tablet (`md`):** sidebar collapses to icon-rail or hides behind hamburger; TopNav persists.
- **Phone:** TopNav only; AppSidebar becomes a `Sheet` drawer; content single-column, `px-4`.
- Used by Dashboard, CreateSite, Admin.

#### 3.4.3 `ProjectDetailLayout` (the split-pane)
The most layout-sensitive screen. Sections: **file tree · editor · context panel** (branch/publish/history/members/settings depending on tab).

- **Desktop (`lg:` ≥1024):** 3 regions —
  - left: `FileTree` (resizable, default 240–280px, min 180, collapsible) via shadcn `resizable` (react-resizable-panels),
  - center: `MonacoEditorPanel` (flex-1),
  - right (optional, `xl:`): context panel (BranchBar/PublishPanel/HistoryTimeline) OR these live as **tabs above the editor** — choose tabs for ≤`lg`, optional right rail for `xl`.
  - `BranchBar` sticky across the top of center+right.
- **Tablet (`md` 768–1024):** split-pane **collapses to tabs.** `TabBar` (Files/Editor · Branches · Publish · History · Members · Settings). FileTree + Editor share the "Files" tab as a 2-pane (tree as a narrow left strip OR a toggle). Editor still usable.
- **Phone (<640):** **no inline tree, no split.** `TabBar` (scrollable) + each section full-screen. Editor tab shows read-only viewer + "edit on larger screen / ask AI" (principle #6); file switching via `MobileFileDrawer`. Publish/History/Branches/Members fully usable (these are the non-engineer-critical flows).
- **Save affordance** is always reachable: desktop in EditorToolbar; tablet/phone as a sticky bottom action bar when editing.

Responsive rule of thumb: **collapse panes → tabs at `< lg`; collapse tree → drawer at `< md`.**

#### 3.4.4 `AdminLayout`
- Same chrome as DashboardLayout but with an **admin sub-nav** (Users / Projects / Quotas / Reserved words) as a secondary `TabBar` (desktop) / `Select` (phone). Guarded to admin role.

---

### 3.5 Pages (wireframe-level)

Routes (Next App Router). `(app)` group = authenticated, guarded; `(auth)` = public.

```
src/app/
  (auth)/login/page.tsx
  (app)/dashboard/page.tsx                 → Dashboard
  (app)/sites/new/page.tsx                 → CreateSite
  (app)/sites/[handle]/page.tsx            → ProjectDetail (default: Files/Editor)
  (app)/sites/[handle]/branches/page.tsx   → ProjectDetail · Branches
  (app)/sites/[handle]/publish/page.tsx    → ProjectDetail · Publish
  (app)/sites/[handle]/history/page.tsx    → ProjectDetail · History
  (app)/sites/[handle]/members/page.tsx    → ProjectDetail · Members
  (app)/sites/[handle]/settings/page.tsx   → ProjectDetail · Settings
  (app)/admin/...                          → Admin (users/projects/quotas/reserved)
```
> Note: ProjectDetail tabs can be either nested routes (above, deep-linkable, recommended) or client tabs. Recommended: **nested routes** so each section is URL-addressable and SSR-prefetchable.

#### Login
```
            ┌──────────────────────────┐
            │        ◢ kotoji          │   ← mark (koto-bridge glyph) + wordmark
            │  あなたのツールに、住処を。 │   ← tagline
            │  ┌────────────────────┐  │
            │  │ Google で続ける     │  │   ← primary button → /auth/google (backend)
            │  └────────────────────┘  │
            │  ─── または ───          │
            │  [ 管理者パスワード… ]    │   ← only if admin-password mode enabled
            │  small print: 社内利用    │
            └──────────────────────────┘
```
- Reads enabled auth modes from backend (`/api/auth/config` or `/api/me` 401 payload). Shows only available providers. Dev/no-auth mode: a clearly-labeled "開発モードで入る" button.
- Responsive: card centered, full-width-minus-16 on phone.

#### Dashboard
```
TopNav: ◢kotoji   [breadcrumb: ダッシュボード]      [⌘K search] [◐theme] [avatar▾]
Sidebar(lg): Dashboard* · (Admin) · ── Recent ── · [+ 新しいサイト]
─────────────────────────────────────────────────────────────────
  ダッシュボード                                   [+ 新しいサイト]
  [search........ /]   [すべて|自分の] [公開中|下書き]
  ┌ ProjectCard ┐ ┌ ProjectCard ┐ ┌ ProjectCard ┐ ┌ ProjectCard ┐
  │ expense-calc│ │ team-wiki   │ │ ...         │ │ ...         │
  │ ●公開中 2h  │ │ ○下書き 1d  │ │             │ │             │
  │ url ⧉       │ │ url ⧉       │ │             │ │             │
  └─────────────┘ └─────────────┘ └─────────────┘ └─────────────┘
```
- Empty → EmptyState ("まだサイトがありません" + 作る CTA). Loading → skeleton cards. Error → InlineAlert + retry.
- Grid: 1/2/3/4 cols at base/`sm`/`lg`/`2xl`.

#### CreateSite
```
  新しいサイトを作る                          [← 戻る]
  ① 始め方を選ぶ   ◉ 空から  ○ Zipから  ○ テンプレート
  ② 名前 (handle)  [ expense-calc            ]  ✓ 使えます
       → https://expense-calc.hosting.example.com
       （小文字・数字・ハイフン / 予約語は不可）
  ③ (Zip選択時) ┌ ドロップゾーン: .zip をここに ┐
                └ または ファイルを選ぶ          ┘  [▓▓▓ 40%]
  ④ 説明 (任意)   [............................]
                                       [ キャンセル ]  [ 作成する ]
```
- Handle validation live (sync rules + async uniqueness, debounced 400ms; shows ✓/✗ with reason). URL preview updates as you type.
- On success → toast + redirect to ProjectDetail.
- `max-w-2xl` single column on all sizes.

#### ProjectDetail (default = Files/Editor)
```
TopNav  | breadcrumb: ダッシュボード / expense-calc
BranchBar:  [バージョン: draft ▾]  preview: expense-calc--draft… ⧉   ●下書きが先行  [ 公開する ]
Tabs(≤lg) / Sidebar(xl): [ファイル] ブランチ 公開 履歴 メンバー 設定
┌──────────┬───────────────────────────────────┬───────────────┐
│ FileTree │ EditorToolbar: index.html ⌘S 差分  │ (xl) context  │
│ ▸ assets │ ┌───────────────────────────────┐ │  rail: publish│
│   index. │ │  Monaco editor                │ │  /history     │
│   style. │ │                               │ │               │
│ ●app.js  │ └───────────────────────────────┘ │               │
│          │ footer: JS · Ln4 Col12 · base@a1b2│               │
└──────────┴───────────────────────────────────┴───────────────┘
```
- **Phone:** BranchBar (compact) → scrollable TabBar → selected section full-screen; Editor tab = read-only + file drawer + "edit elsewhere / ask AI".
- Save = commit to current branch with base SHA; conflict → ConflictResolver inline.

#### ProjectDetail · Publish
```
  公開                                  現在: ●下書きが先行（未公開の変更3件）
  これから公開される変更:
  ┌ DiffViewer (draft ↔ published, サマリ) ┐   +24 −6
  └────────────────────────────────────────┘
  公開メッセージ(任意) [ 価格表を更新 ............ ]
  ⚠ 公開すると expense-calc.hosting.example.com に即反映されます。後で戻せます。
                                  [ プレビューを見る ] [ 公開する ▸gold ]
```
- ConfirmDialog before publish. Progress while running. Success → gold glint + toast "公開しました 🎍". If PR-delegation mode: button = "公開をリクエスト".

#### ProjectDetail · History
```
  履歴                          [バージョン:draft▾] [ソース: すべて▾]
  ── 今日 ──
   ◍ 価格表を更新          a1b2c3  Editor   2h前   [差分][戻す]
   ◍ ロゴ差し替え          9f8e7d  MCP(AI)  4h前   [差分][戻す]
  ── 昨日 ──
   ◍ 初回アップロード       0011aa  Upload   1d前   [差分]
                                                  [◂ 前へ  次へ ▸]
```
- Roll back → ConfirmDialog → mutation → toast. Diff opens DiffViewer (side-by-side desktop, unified phone).

#### Admin
```
  管理                [ ユーザー | プロジェクト | クォータ | 予約語 ]
  (Users)   table: avatar/name/email/role/last-seen  [+招待]
  (Projects) table: handle/owner/status/size/updated  [...]
  (Quotas)  per-user/site limits form
  (Reserved) editable reserved-word list (draft/api/admin/...)
```
- Admin-guarded (role from `/api/me`). Tables → stacked cards on phone.

---

## 4. Data layer, auth, theming, a11y, conventions

This section makes the design *runnable* — the contract between the React UI and the Go backend.

### 4.1 API client (single source of truth across the language split)

The Go↔Next type-sync is the central cost of the two-language stack. Strategy:

1. **OpenAPI is authoritative.** Backend authors/maintains `docs/contracts/openapi.yaml` alongside Go handlers (the repo already has `docs/contracts/`). It describes every REST endpoint, request/response schema, and error envelope.
2. **Generate a typed TS client** into the frontend from that spec — recommended: **`openapi-typescript`** (emits `src/lib/api/schema.d.ts` types) + **`openapi-fetch`** (tiny typed fetch client). This gives compile-time-safe calls with zero hand-written DTOs. Run via an npm script `gen:api` (and a CI check that the committed client matches the spec).
3. **No hand-maintained duplicate types.** Zod schemas exist only for *forms* (client-side validation) and, where useful, are derived from / cross-checked against the generated types. Server is the validation authority; client zod is UX sugar.

```ts
// src/lib/api/client.ts
import createClient from "openapi-fetch";
import type { paths } from "./schema"; // generated by openapi-typescript

export const api = createClient<paths>({
  baseUrl: "/api",            // NPM routes /api → Go backend
  credentials: "include",     // send the opaque session cookie
});
```

**Error envelope (frontend assumes this shape; backend must honor it):**
```jsonc
// non-2xx body
{ "error": { "code": "conflict", "message": "...", "details": { "baseSha": "...", "currentSha": "..." } } }
```
A shared `ApiError` class normalizes this; TanStack Query `error` carries `code` so UI can branch (e.g. `code === "conflict"` → ConflictResolver; `401` → redirect to login; `403` → not-authorized state).

### 4.2 TanStack Query patterns

- **One `QueryClient`** in a client `Providers` component (also hosts ThemeProvider, TooltipProvider, Toaster). SSR: use `HydrationBoundary` + `prefetchQuery` in server components for first paint of Dashboard/ProjectDetail.
- **Query key convention:** `["sites"]`, `["site", handle]`, `["files", handle, branch]`, `["file", handle, branch, path]`, `["log", handle, branch]`, `["diff", handle, from, to]`, `["me"]`, `["admin","users"]`. Centralize in `src/lib/api/keys.ts`.
- **Custom hooks per resource** in `src/lib/api/hooks/` — `useSites()`, `useSite(handle)`, `useFiles(handle, branch)`, `useFileContent(handle, branch, path)`, `useSaveFile()`, `usePublish()`, `useLog()`, `useDiff()`, `useRollback()`, `useMe()`. Components never call `api` directly; they use hooks. This is the testable seam on the frontend (mock the hooks).
- **Mutations:**
  - `useSaveFile` sends `{ path, content, baseSha }`. On `conflict` error → do *not* optimistically apply; surface ConflictResolver. On success → toast + invalidate `["file",…]`, `["log",…]`, `["site",handle]`.
  - `usePublish` → optimistic publish status pill is acceptable but reconcile on settle; invalidate site + diff.
  - All mutations: `onError` → sonner error toast with the server `message`; loading → button `loading`.
- **Stale/refetch:** `staleTime: 30s` default; `["me"]` longer (5m). Refetch on window focus only for list/status (sites, log), not for file content (avoid clobbering an open editor).
- **Loading / error / empty are mandatory triplets** for every list/detail:
  - loading → Skeleton mirroring layout,
  - error → InlineAlert (or full EmptyState-error) + retry button (`refetch`),
  - empty → EmptyState with a primary action,
  - success → content.
  A small `<QueryState query=… skeleton=… empty=…>` helper standardizes this.

### 4.3 Auth flow on the client

- **Login:** Login page buttons link to backend OIDC endpoints (`/auth/google` etc.). The browser navigates to the Go backend, which runs OIDC, sets an **opaque session cookie** (HttpOnly, Secure, SameSite=Lax), and redirects back to `/dashboard`. Frontend never handles tokens.
- **Session read:** `useMe()` calls `GET /api/me`. `200` → `{ user, role, authMode }`; `401` → not authenticated.
- **Route guard:** a server-side check in the `(app)` layout (read session cookie / call `/api/me` from the server component) redirects unauthenticated users to `/login`. Client-side, an `AuthGate` shows a spinner while `useMe()` resolves and redirects on 401. Admin routes additionally check `role === "admin"` → else 403 page.
- **Logout:** `POST /auth/logout` (clears server session + cookie) → redirect `/login`. UserMenu "サインアウト".
- **Auth modes surfaced:** `/api/me` (or `/api/auth/config`) reports `authMode` (`oidc`/`admin-password`/`none`) and available providers so Login renders only what's enabled. Dev/no-auth shows the explicit dev-entry button.

### 4.4 Theming (next-themes)

- `<ThemeProvider attribute="class" defaultTheme="system" enableSystem disableTransitionOnChange>` in `Providers`. Class strategy matches `@custom-variant dark (&:is(.dark *))`.
- **No-flash:** rely on next-themes' inline script (App Router: it injects the script; ensure `suppressHydrationWarning` on `<html>`). ThemeToggle reads/sets `theme`; guard against hydration mismatch with a mounted flag.
- **Monaco theme follows app theme:** subscribe to resolvedTheme and call `monaco.editor.setTheme("kotoji-dark"|"kotoji-light")`; both themes are defined from the editor tokens (§2.1.5) at editor mount.

### 4.5 Iconography

- `lucide-react`, imported per-icon (tree-shakes). Central re-export `src/components/atoms/icon.tsx` only for the constrained `<Icon name size>` wrapper; most usage imports the lucide component directly inside an atom/molecule.
- Canonical mapping (excerpt): nav Dashboard=`LayoutGrid`, New=`Plus`, search=`Search`, copy=`Copy`/`Check`, branch/version=`GitBranch`(internal) shown as "version"; publish=`Rocket` or `Upload`; history=`History`; rollback=`Undo2`; members=`Users`; settings=`Settings2`; theme=`Sun`/`Moon`/`Monitor`; MCP/AI source=`Sparkles`; upload=`UploadCloud`; file types per FileTypeIcon; external link=`ArrowUpRight`; conflict/warning=`TriangleAlert`; success=`CircleCheck`.
- Meaningful icons (icon-only buttons, status) carry `aria-label`; decorative ones `aria-hidden="true"`. Status is never icon/color alone — always paired text or sr-only text.

### 4.6 Monaco specifics

- Loaded via `@monaco-editor/react`, **`dynamic(() => …, { ssr: false })`** + Suspense skeleton. Worker setup per Next 16 (use `@monaco-editor/react`'s loader or self-hosted workers; configure to avoid CDN in self-host).
- Languages: html/css/javascript/json/markdown by file extension; plain text fallback.
- Read-only on phone and on any non-writable branch/role. Diff via `DiffEditor`.
- Save = ⌘S/Ctrl+S bound; warns on navigate-away with unsaved changes (beforeunload + router guard). Base SHA captured at file open, sent with every save.
- Custom themes `kotoji-light`/`kotoji-dark` defined from CSS tokens so the code surface matches chrome.

### 4.7 Motion

- Library: **`tw-animate-css`** (Tailwind v4-native replacement for tailwindcss-animate) + Radix data-state-driven animations for popover/dialog/sheet/tooltip.
- Standard transitions: color/opacity 120–180ms ease-out; dialog/sheet enter 180ms (fade+slight slide/scale), exit 120ms; toast slide-in 180ms; hover-lift on cards `translate-y-[-2px]` 120ms.
- **Reduced motion:** global guard —
  ```css
  @media (prefers-reduced-motion: reduce) {
    *, *::before, *::after { animation-duration:.001ms !important; transition-duration:.001ms !important; animation-iteration-count:1 !important; scroll-behavior:auto !important; }
  }
  ```
  Spinner switches to opacity pulse; the publish gold-glint is suppressed (just a static success state).

### 4.8 Accessibility checklist (WCAG AA — enforced, not optional)

- **Contrast:** all token pairings meet AA (verified in §2). Never convey state by color alone (status badges always have text/icon; diff uses +/− glyphs, not just red/green).
- **Keyboard:** every interactive element reachable & operable by keyboard. Logical tab order. FileTree full arrow-key nav. CommandPalette ⌘K. Editor Save ⌘S. Esc closes overlays. No keyboard traps (Radix handles focus traps in dialogs/sheets correctly).
- **Focus visible:** 3px `--ring` focus ring on every focusable (`focus-visible`), never removed without replacement. Sufficient offset on dark.
- **Labels & roles:** all inputs have `<Label htmlFor>`; icon-only buttons have `aria-label`; status uses `role="status"`/`aria-live="polite"` for async results (save/publish/copy toasts via sonner are announced).
- **Forms:** errors tied via `aria-describedby`, `aria-invalid`; error summary focusable; required marked in text+sr, not color.
- **Landmarks:** `<header>`(TopNav), `<nav>`(sidebar), `<main>`, `<footer>`; skip-to-content link as first focusable.
- **Live regions:** publish/save progress and conflict announcements via polite live region.
- **Reduced motion:** honored globally (§4.7).
- **Target size:** ≥44×44 touch targets (§2.4).
- **Language:** `<html lang="ja">` (switchable); mixed-language content marked where needed.
- **Testing:** lint with `eslint-plugin-jsx-a11y`; smoke with `@axe-core/react` in dev; manual keyboard pass on each template.

### 4.9 Components folder & naming convention

```
src/
  app/                       # routes & layouts (App Router)
  components/
    ui/                      # shadcn-GENERATED primitives (button, input, dialog, …) — do not hand-edit beyond shadcn
    atoms/                   # kotoji atoms wrapping ui/ (status-badge, kbd, code-text, spinner, icon, …)
    molecules/               # form-field, search-bar, branch-select, project-card, copyable-url, confirm-dialog, empty-state, …
    organisms/               # top-nav, app-sidebar, file-tree, monaco-editor-panel, publish-panel, history-timeline, …
    templates/               # auth-layout, dashboard-layout, project-detail-layout, admin-layout
  lib/
    api/                     # client.ts, schema.d.ts(generated), keys.ts, hooks/, error.ts
    auth/                    # auth-gate, useMe wrapper, guards
    utils.ts                 # cn(), formatRelativeTime(), validateHandle(), etc.
    monaco/                  # theme defs, language map
  styles/ (globals.css)      # @theme tokens
  hooks/                     # generic UI hooks (useMediaQuery, useCopyToClipboard, useDebounce)
```

**Naming:**
- Files: `kebab-case.tsx`. Components: `PascalCase`. Hooks: `useCamelCase`.
- One component per file; co-locate variant CVA + types in the same file.
- shadcn primitives stay in `ui/` and are imported by atoms/molecules; **app code imports from atoms/molecules/organisms, not directly from `ui/`** (except trivial cases) — this keeps the atomic layering and lets us re-skin primitives in one place.
- Tests: `*.test.tsx` co-located; organisms tested with a mocked QueryClient + mocked hooks; atoms/molecules with React Testing Library + jest-axe.
- `cn()` (clsx + tailwind-merge) is the only class-composition helper; CVA for variant-driven atoms.

### 4.10 Responsive helpers

- `useMediaQuery` / `useBreakpoint()` (matchMedia, SSR-safe with a mounted default) drives JS-level layout switches (split-pane↔tabs, side-by-side↔unified diff, sidebar↔sheet). Prefer CSS (`lg:` etc.) for pure layout; use JS only where a component genuinely renders differently (Monaco diff mode, FileTree inline vs drawer).
- Canonical hook values map to the three bands: `isPhone` (<640), `isTablet` (640–1023), `isDesktop` (≥1024).

---

## 5. Open questions / gaps (考慮漏れ — needs a decision)

These are surfaced deliberately; the design above makes reasonable defaults but these should be resolved before/while building.

1. **i18n library & default language.** Spec implies ja-first, en optional. Not chosen: `next-intl` (consistent with your tsumo/shop/uma stack) vs `react-i18next`. *Recommendation:* `next-intl` (your house standard, App-Router-native). Decide message-key structure and whether the UI is bilingual at launch or ja-only first. All copy above assumes string keys, not literals.

2. **Roles & permissions model.** UI assumes owner/editor/viewer + admin, but the spec only names "メンバー/権限" and "管理者." Exact role set, what each can do (who can publish? can viewers see drafts? per-branch perms for AI feature-* branches?), and whether MCP token scope == a role need defining. RoleSelect/MemberTable/route guards depend on this.

3. **MCP token management UI.** Spec has per-project-scoped MCP tokens, but there's no screen designed for *issuing/revoking* them. Likely a "Connect AI / MCP" panel in ProjectDetail or Settings (token create, copy-once, revoke, show connection URL). Needs to be added to the inventory once the token model is fixed (one token per user-per-project? expiry? scopes beyond project?).

4. **"Request publish" vs direct publish — which mode, when, and how is it surfaced.** Spec says non-engineers see only "公開リクエスト" with PR delegated to GitHub; but also describes a direct draft→published action. Is the mode per-project, per-role, or a global setting? PublishPanel renders differently in each. Also: how does the UI reflect *pending* publish requests / PR status from GitHub (webhook-driven)? A "公開リクエスト中" state and where it shows (badge? notifications?) is undefined.

5. **GitHub-driven state & notifications.** Merge→webhook→server pull→redeploy is async and external. The UI needs to represent "published just changed via GitHub," possibly a notifications surface, and reconcile optimistic local state with externally-driven changes. No notification/activity UI is currently specified.

6. **Brand assets.** The "koto-bridge glyph" mark, favicon, OG image, and the empty-state line illustration are referenced but not yet designed. Need an actual SVG mark and a small illustration set (light/dark variants). Gold-leaf success glint needs a concrete visual spec (e.g. a brief shimmer on the success icon).

7. **File/branch write affordances on mobile.** Principle #6 deliberately makes phones read-only for code editing. Confirm this is acceptable, and design the exact "ask AI to edit" / "open on larger screen" affordance (does it deep-link? email a link? just instruct?). Also: can non-engineers create/delete *files* (not just edit) on tablet, and what's the new-file/rename/delete UX (currently only context-menu noted)?

8. **Upload limits & feedback specifics.** Client knows soft limits but the exact numbers (max zip size, max file count, allowed extensions) live server-side. The UI should fetch these (config endpoint) to show accurate pre-upload guidance and post-rejection messages, rather than hard-coding. That config endpoint isn't defined.

9. **Conflict/optimistic-lock UX edge cases.** ConflictResolver covers single-file save conflicts. Undefined: multi-file conflicts, conflict during publish (published moved underneath), and what "overwrite" actually does at the git level (force? new commit on top?). Needs backend semantics confirmed so the UI copy is truthful.

10. **Empty/first-run & onboarding.** No first-login onboarding designed (the 3-minute self-host trial promise). Worth a minimal guided empty state ("create your first site → upload a zip → get a URL") and possibly a sample/template project.

11. **OpenAPI ownership & drift CI.** The single-source-of-truth approach (§4.1) only works if there's a CI gate ensuring the committed generated client matches the spec and the spec matches the Go handlers. The generation/verification pipeline (and whether Go types are generated *from* the spec or the spec is hand-written) is a process decision still open. `docs/contracts/` currently exists but is empty.

12. **Thumbnail/preview rendering for ProjectCard.** Card mentions a thumbnail; generating a live preview/screenshot of an arbitrary hosted HTML page (sandboxed!) is non-trivial and a potential security surface. Decide: static placeholder by status, generated screenshot (sandboxed headless), or none. Affects ProjectGrid visual density.

---

*End of design.md — this is the implementation source of truth for the kotoji frontend. Update tokens here first, then propagate to `globals.css`; update the inventory here first, then scaffold components.*
