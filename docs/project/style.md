# Dorf Visual Style

This is the visual authority for Dorf. The source artwork is
[`assets/logo.png`](../../assets/logo.png) and [`assets/cover.png`](../../assets/cover.png); new
interfaces should extend their character without redrawing or competing with them.

## Character

Dorf should feel like a **calm control plane in a cozy workshop**: grounded, capable, welcoming,
and quietly technical. The village, paths, lit windows, tools, and greenery communicate independent
workspaces connected by deliberate infrastructure. The experience should feel owned and inhabited,
not remote, sterile, or corporate.

Prefer:

- deep natural backgrounds with warm, restrained highlights;
- clear hierarchy, compact composition, and generous breathing room;
- muted explanatory text with emphasis reserved for the current choice or important state;
- tactile pixel artwork and terminal-native geometry derived from the source assets; and
- plain, human language that reports what Dorf is doing.

Avoid generic neon agent imagery, glossy gradients, excessive glow, cold blue-purple SaaS palettes,
decorative complexity, and large areas of saturated green or amber.

## Palette

The palette is role-based rather than a requirement that every surface use every color.

| Role | Color | Use |
| --- | --- | --- |
| Night forest | `#192F23` | Primary dark field and deep visual depth |
| Forest | `#30452B` | Panels and quieter elevated surfaces |
| Moss teal | `#335446` | Secondary structure and cool natural contrast |
| Leaf moss | `#678C28` | Selected, healthy, or active accents used sparingly |
| Warm lichen | `#D5C592` | Dorf wordmark and warm primary emphasis |
| Path cream | `#F5E5C7` | High-contrast text and light paths through dark layouts |
| Muted sage | `#8F9D8D` | Descriptions, supporting labels, and the tagline |
| Hearth amber | `#C47D1F` | Attention, warmth, and small focal highlights |

Use cream and warm lichen for legibility, not as large backgrounds. Amber should behave like a lit
window: small enough to remain meaningful. Green communicates the environment before it
communicates status; always pair status color with text or a symbol.

## Artwork and wordmark

- Preserve the rounded village silhouette, visible paths, surrounding foliage, and single warm
  hearth as the logo's essential composition.
- Keep pixel edges deliberate. Do not blur, smooth, trace, or mix the artwork with unrelated icon
  styles.
- Use the lowercase pixel `dorf` wordmark from the cover. Do not substitute an invented display
  face when the branded wordmark is intended.
- Let scenes tell a small operational story: separate workshops, visible work, and paths connecting
  them. Empty decoration should not outweigh the product information.
- Prefer one strong illustration or lockup over repeated small brand ornaments.

Terminal artwork must be generated from the source assets. The setup banner uses colored Braille
for the logo, full-cell blocks for the wordmark, warm lichen for `dorf`, and muted sage for the
tagline. Edit the source assets or generator, then run `mise run brand:generate`; do not hand-edit
`internal/brand/terminal_generated.go`.

## Interface presentation

- Start with the situation or decision the user is facing, followed by one muted sentence of
  context.
- Keep ordinary questions and headings on the native surface; do not turn them into colored badges.
- Group choices by their real distinction, such as local and cloud, without turning every label
  into a badge.
- Keep the primary path visually obvious. Secondary controls and help belong in muted text.
- Use sentence case and short verbs. Report concrete states such as `Creating Sandbox` and
  `PostgreSQL ready` rather than internal executor terminology.
- Animation and live refresh should communicate progress, not create ambient motion.
- In terminals, respect `NO_COLOR` and `TERM=dumb`; layouts must remain understandable without
  color and should degrade cleanly at narrow widths.

The tagline is **Your agents. Your infrastructure. One API.** Keep it visually subordinate to the
logo and wordmark, centered beneath their combined lockup when they appear together.
