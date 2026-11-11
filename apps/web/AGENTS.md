<!-- BEGIN:nextjs-agent-rules -->

## This is NOT the Next.js you know

This version has breaking changes — APIs, conventions, and file structure may all differ from your training data. Read the relevant guide in `node_modules/next/dist/docs/` (resolved from this file's directory; in monorepos the `next` package may not be visible from the repo root) before writing any code. Heed deprecation notices.

This block is written and re-added by `next dev` — verify at `node_modules/next/dist/server/lib/generate-agent-files.js`. Removing it from a diff only re-creates the uncommitted change; committing it with your work keeps the tree clean.

<!-- END:nextjs-agent-rules -->

# Dorf's page

The components in `components/ui` come from [Fluid Functionalism](https://www.fluidfunctionalism.com)
(MIT, © Micka Touillaud). Before changing one or adding motion, read its skill at
https://github.com/mickadesign/fluid-functionalism/tree/main/skills/fluid-functionalism and the
section of `references/craft.md` the component's docs link to. Dorf's own parts are in
`components/dorf`, `lib/dorf.ts` and `app/page.tsx`. `bun run build` writes the static export the
agent serves; `DORF_AGENT=http://127.0.0.1:8091 bun run dev` proxies the API to a running agent.
