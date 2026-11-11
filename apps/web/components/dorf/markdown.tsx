import { useMemo, type ComponentProps } from "react"
import ReactMarkdown, { type Components, type ExtraProps } from "react-markdown"
import remarkGfm from "remark-gfm"
import { Code } from "@/components/dorf/code"
import { Mermaid } from "@/components/dorf/mermaid"
import type { Picture } from "@/components/dorf/image-dialog"
import { View, ViewPending } from "@/components/dorf/view"

const code = ({ className, children, ...p }: ComponentProps<"code"> & ExtraProps) => {
  const lang = /language-(\S+)/.exec(className ?? "")?.[1]
  const text = String(children ?? "")
  if (lang || text.includes("\n")) return <Code code={text.replace(/\n$/, "")} lang={lang ?? "text"} />
  return <code className="rounded bg-surface-3 px-1 py-0.5 font-mono text-[0.9em]" {...p}>{children}</code>
}

const components: Components = {
  p: (p) => <p className="my-2 first:mt-0 last:mb-0" {...p} />,
  ul: (p) => <ul className="my-2 list-disc space-y-1 pl-5" {...p} />,
  ol: (p) => <ol className="my-2 list-decimal space-y-1 pl-5" {...p} />,
  li: (p) => <li className="[&>p]:my-0" {...p} />,
  h1: (p) => <h3 className="mt-4 mb-2 font-semibold" {...p} />,
  h2: (p) => <h3 className="mt-4 mb-2 font-semibold" {...p} />,
  h3: (p) => <h3 className="mt-3 mb-1.5 font-semibold" {...p} />,
  strong: (p) => <strong className="font-semibold text-foreground" {...p} />,
  a: (p) => <a className="text-[color:var(--fluid-accent)] underline underline-offset-2" target="_blank" rel="noopener noreferrer" {...p} />,
  blockquote: (p) => <blockquote className="my-2 border-l-2 border-border pl-3 text-muted-foreground" {...p} />,
  code,
  pre: ({ children }) => <>{children}</>,
  table: (p) => <div className="my-2 overflow-x-auto"><table className="text-left text-sm" {...p} /></div>,
  th: (p) => <th className="border-b border-border px-2 py-1 font-semibold" {...p} />,
  td: (p) => <td className="border-b border-border/50 px-2 py-1 align-top" {...p} />,
}

// The image opens in the viewer, focused the way an attachment's thumbnail is.
export const OPENS = "block w-full cursor-zoom-in rounded-md outline-none focus-visible:ring-1 focus-visible:ring-[color:var(--focus-ring,#6B97FF)]"

// An image shows only when the agent read that file in this Thread, by path or file name: the page loads nothing
// from elsewhere, so a reply cannot leak data through an image URL. Any other image shows as its alt text. A click
// opens the image in the viewer.
export function Markdown({ children, images, onImageOpen }: { children: string; images?: Map<string, string>; onImageOpen?: (image: Picture) => void }) {
  const img: Components["img"] = ({ src, alt }) => {
    const path = typeof src === "string" ? src : ""
    const seen = images?.get(path) ?? images?.get(path.split("/").pop() ?? "")
    if (!seen) return <span className="text-muted-foreground">[{alt || path}]</span>
    const name = alt || path.split("/").pop() || "image"
    return (
      <span className="my-2 block">
        <button type="button" onClick={() => onImageOpen?.({ src: seen, name })} aria-label={`Open ${name}`} className={OPENS}>
          {/* eslint-disable-next-line @next/next/no-img-element -- a data URL from the transcript, not an optimizable asset */}
          <img src={seen} alt={alt ?? ""} className="max-h-[70vh] w-full rounded-md border border-border object-contain object-top" />
        </button>
        {alt && <span className="mt-1 block font-mono text-xs text-muted-foreground">{alt}</span>}
      </span>
    )
  }
  // A `view` block becomes a sandboxed frame, and a `mermaid` block a diagram, once its closing fence arrives; until
  // then it is still streaming. The component stays the same while the text does, so React keeps each frame and its
  // state.
  const view = useMemo(
    () =>
      function ViewOrCode(p: ComponentProps<"code"> & ExtraProps) {
        const lang = /\blanguage-(view|mermaid)\b/.exec(p.className ?? "")?.[1]
        if (!lang) return code(p)
        const at = p.node?.position
        const raw = at ? children.slice(at.start.offset, at.end.offset) : ""
        const closed = /\n[ \t]*(`{3,}|~{3,})[ \t]*$/.test(raw)
        const text = String(p.children ?? "")
        if (lang === "mermaid") return closed ? <Mermaid code={text.replace(/\n$/, "")} /> : code(p)
        return closed ? <View html={text} /> : <ViewPending />
      },
    [children],
  )
  return (
    <div className="whitespace-normal">
      <ReactMarkdown remarkPlugins={[remarkGfm]} components={{ ...components, img, code: view }}>{children}</ReactMarkdown>
    </div>
  )
}
