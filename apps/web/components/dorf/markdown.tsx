import ReactMarkdown, { type Components } from "react-markdown"
import remarkGfm from "remark-gfm"
import { Code } from "@/components/dorf/code"

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
  code: ({ className, children, ...p }) => {
    const lang = /language-(\S+)/.exec(className ?? "")?.[1]
    const text = String(children ?? "")
    if (lang || text.includes("\n")) return <Code code={text.replace(/\n$/, "")} lang={lang ?? "text"} />
    return <code className="rounded bg-surface-3 px-1 py-0.5 font-mono text-[0.9em]" {...p}>{children}</code>
  },
  pre: ({ children }) => <>{children}</>,
  table: (p) => <div className="my-2 overflow-x-auto"><table className="text-left text-sm" {...p} /></div>,
  th: (p) => <th className="border-b border-border px-2 py-1 font-semibold" {...p} />,
  td: (p) => <td className="border-b border-border/50 px-2 py-1 align-top" {...p} />,
}

export function Markdown({ children }: { children: string }) {
  return (
    <div className="whitespace-normal">
      <ReactMarkdown remarkPlugins={[remarkGfm]} components={components}>{children}</ReactMarkdown>
    </div>
  )
}
