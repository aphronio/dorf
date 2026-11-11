"use client"

import { type ReactNode, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react"
import { Button } from "@/components/ui/button"
import { ChatMessage } from "@/components/ui/chat-message"
import { ThinkingIndicator } from "@/components/ui/thinking-indicator"
import { Markdown } from "@/components/dorf/markdown"
import { ToolCalls } from "@/components/dorf/tool-call"
import { useIcons } from "@/lib/icon-context"
import { ImageDialog } from "@/components/dorf/image-dialog"
import { type Block, type Entry, imageFile, type Live, type Message, type Queued, text } from "@/lib/dorf"

type Call = Extract<Block, { type: "toolCall" }>
type Output = { text: string; error?: boolean; running?: boolean; images?: string[] }
type Item =
  | { kind: "user"; key: string; text: string; files: File[] }
  | { kind: "text"; key: string; text: string; images?: Map<string, string> }
  | { kind: "calls"; key: string; calls: Call[] }
  | { kind: "note"; key: string; text: string; error?: boolean }
  | { kind: "subagent"; key: string; name: string; text: string; error?: boolean }

const command = (c: Call) => {
  const a = c.arguments ?? {}
  if (c.name === "bash") return String(a.command ?? "")
  if (c.name === "subagent") return `subagent ${a.name}${a.message ? `: ${a.message}` : ""}`
  return `${c.name} ${JSON.stringify(a)}`
}


// Consecutive commands form one block, even across the assistant messages Pi splits them into. The items before are
// left as they are: they may be the settled ones, kept from render to render.
function push(items: Item[], key: string, message: Message) {
  const blocks = typeof message.content === "string" ? [{ type: "text", text: message.content } as Block] : message.content
  blocks.forEach((b, i) => {
    const last = items.at(-1)
    if (b.type === "toolCall") {
      if (last?.kind === "calls") items[items.length - 1] = { ...last, calls: [...last.calls, b] }
      else items.push({ kind: "calls", key: `${key}.${i}`, calls: [b] })
    } else if (b.type === "text" && b.text.trim()) items.push({ kind: "text", key: `${key}.${i}`, text: b.text })
  })
  if (message.stopReason === "error") items.push({ kind: "note", key: `${key}.e`, text: message.errorMessage ?? "The model failed.", error: true })
}

// A reply places the images read before it, by path and by file name; a later read of the same name leaves it as it
// was. Places them in the items from `from` on, after the images placed before; returns the images placed by then.
function place(items: Item[], from: number, outputs: Record<string, Output>, images: Map<string, string>) {
  for (const item of items.slice(from)) {
    if (item.kind === "text") item.images = images
    if (item.kind !== "calls") continue
    for (const c of item.calls) {
      const src = outputs[c.id]?.images?.[0]
      const path = String(c.arguments?.path ?? "")
      if (src && path) images = new Map(images).set(path, src).set(path.split("/").pop()!, src)
    }
  }
  return images
}

const EXTENSION: Record<string, string> = { "image/png": "png", "image/jpeg": "jpg", "image/gif": "gif", "image/webp": "webp" }

// The items of the stored entries. They change when an entry arrives, not with each token of a reply.
function settle(entries: Entry[]) {
  const outputs: Record<string, Output> = {}
  const items: Item[] = []
  for (const e of entries) {
    const m = e.model?.[0]
    if (!m) continue
    if (e.kind === "pi.tool-result" && m.toolCallId) {
      const images = typeof m.content === "string" ? [] : m.content.flatMap((b) => (b.type === "image" ? [`data:${b.mimeType};base64,${b.data}`] : []))
      outputs[m.toolCallId] = { text: text(m.content), error: m.isError, images }
    }
    else if (e.kind === "pi.user") {
      const sent = typeof m.content === "string" ? [] : m.content.flatMap((b) => (b.type === "image" ? [b] : []))
      const files = sent.map((b, i) => imageFile(b, `image ${i + 1}.${EXTENSION[b.mimeType] ?? "png"}`))
      items.push({ kind: "user", key: String(e.id), text: text(m.content), files })
    }
    else if (e.kind === "dorf.subagent-report") {
      // The model reads "[subagent name answered…] text"; the page shows the author once.
      const { name, failed } = e.data as { name: string; failed?: true }
      const body = text(m.content)
      items.push({ kind: "subagent", key: String(e.id), name, text: failed ? body : body.slice(body.indexOf("] ") + 2), error: failed })
    }
    else if (e.kind === "pi.assistant") push(items, String(e.id), m)
    else if (e.kind === "pi.compaction") items.push({ kind: "note", key: String(e.id), text: "Earlier conversation summarized" })
  }
  return { outputs, items, images: place(items, 0, outputs, new Map()) }
}

type TranscriptProps = { entries: Entry[]; live: Live; queued?: Queued[]; onSubagent?: (name: string) => void; loadEarlier?: () => Promise<void> }

export function Transcript({ entries, live, queued = [], onSubagent, loadEarlier }: TranscriptProps) {
  const [opened, setOpened] = useState<File>()
  const scroller = useRef<HTMLDivElement>(null)
  const pinned = useRef(true)
  const [loading, setLoading] = useState(false)
  const [loadFailed, setLoadFailed] = useState(false)
  // Set while earlier entries load: the distance from the bottom to keep once they land above.
  const fromBottom = useRef<number>(undefined)
  const oldest = useRef<number>(undefined)
  const top = useRef<HTMLDivElement>(null)

  const earlier = useCallback(async () => {
    const el = scroller.current
    if (!el || !loadEarlier) return
    fromBottom.current = el.scrollHeight - el.scrollTop
    setLoading(true)
    setLoadFailed(false)
    await loadEarlier().catch(() => {
      fromBottom.current = undefined
      setLoadFailed(true)
    })
    setLoading(false)
  }, [loadEarlier])
  // Infinite scroll upward: earlier entries load as the top comes near, and again while it stays in view.
  useEffect(() => {
    const el = top.current
    if (!el || !scroller.current || !loadEarlier || loading || loadFailed) return
    const seen = new IntersectionObserver(([e]) => e.isIntersecting && void earlier(), { root: scroller.current, rootMargin: "600px 0px 0px 0px" })
    seen.observe(el)
    return () => seen.disconnect()
  }, [earlier, loadEarlier, loading, loadFailed])

  const settled = useMemo(() => settle(entries), [entries])
  const outputs = { ...settled.outputs }
  const items = settled.items.slice()
  for (const t of live.tools ?? []) if (t.status !== "done") outputs[t.callId] = { text: t.output ?? "", running: true }
  const generating = live.generation?.message
  // A reply still coming places the images read before it; its own calls have not returned any yet.
  if (generating) {
    push(items, "live", generating)
    place(items, settled.items.length, outputs, settled.images)
  }
  if (live.run && !text(generating?.content).trim() && !Object.values(outputs).some((o) => o.running))
    items.push({ kind: "note", key: "thinking", text: "" })
  if (live.generation?.retry) items.push({ kind: "note", key: "retry", text: `Retrying: ${live.generation.retry.error}` })
  if (live.runFailure && !live.run)
    items.push({ kind: "note", key: "failed", error: true, text: `Run ended: ${live.runFailure.reason}${live.runFailure.detail ? `, ${live.runFailure.detail}` : ""}` })

  useLayoutEffect(() => {
    const el = scroller.current
    if (!el) return
    if (fromBottom.current !== undefined && entries[0]?.id !== oldest.current) {
      el.scrollTop = el.scrollHeight - fromBottom.current
      fromBottom.current = undefined
    } else if (pinned.current) el.scrollTop = el.scrollHeight
    oldest.current = entries[0]?.id
  })
  // The composer growing shrinks this area; stay at the bottom if already there.
  useEffect(() => {
    const el = scroller.current
    if (!el) return
    const ro = new ResizeObserver(() => {
      if (pinned.current) el.scrollTop = el.scrollHeight
    })
    ro.observe(el)
    return () => ro.disconnect()
  }, [])

  return (
    <div
      ref={scroller}
      onScroll={(e) => {
        const el = e.currentTarget
        pinned.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40
      }}
      className="min-h-0 flex-1 overflow-y-auto"
    >
      <div className="mx-auto flex w-full max-w-3xl flex-col gap-4 px-4 py-6">
        {loadEarlier && <div ref={top} />}
        {loading && <p className="text-center text-xs text-muted-foreground">Loading earlier…</p>}
        {loadFailed && (
          <Button variant="ghost" size="compact" className="self-center text-red-600 dark:text-red-400" onClick={() => void earlier()}>
            Couldn&apos;t load earlier. Retry
          </Button>
        )}
        {items.map((item) => {
          switch (item.kind) {
            case "user":
              return (
                <ChatMessage key={item.key} from="user" className="whitespace-pre-wrap" files={item.files} onFileOpen={setOpened}>
                  {item.text}
                </ChatMessage>
              )
            case "text":
              // A reply spans the column, so a view in it does too, however short its text.
              return <ChatMessage key={item.key} from="assistant" className="max-w-full self-stretch items-stretch"><Markdown images={item.images}>{item.text}</Markdown></ChatMessage>
            case "calls": {
              // Images a tool returned, such as screenshots the agent read, show small here; a reply places them full size.
              // A subagent call waits for the subagent's answer; its link opens the subagent's transcript.
              const waiting = item.calls.filter((c) => c.name === "subagent" && outputs[c.id]?.running)
              const shots = item.calls.flatMap((c) => (outputs[c.id]?.images ?? []).map((src, i) => ({ src, key: `${c.id}.${i}`, name: String(c.arguments?.path ?? "").split("/").pop() })))
              return (
                <div key={item.key} className="-mx-2">
                  <ToolCalls
                    calls={item.calls.map((c) => ({ id: c.id, command: command(c), shell: c.name === "bash", ...outputs[c.id], output: outputs[c.id]?.text }))}
                  />
                  {waiting.map((c) => (
                    <SubagentLink key={c.id} name={String(c.arguments?.name)} onSubagent={onSubagent} className="px-2 py-1 text-xs">
                      waiting on {String(c.arguments?.name)}
                    </SubagentLink>
                  ))}
                  {shots.length > 0 && (
                    <div className={`mt-2 grid gap-2 px-2 ${shots.length > 1 ? "sm:grid-cols-2" : ""}`}>
                      {shots.map((shot) => (
                        <figure key={shot.key} className="min-w-0">
                          {/* eslint-disable-next-line @next/next/no-img-element -- a data URL from the transcript, not an optimizable asset */}
                          <img src={shot.src} alt={shot.name || "screenshot"} className="max-h-40 w-auto max-w-full rounded-md border border-border object-contain object-left-top" />
                          {shot.name && <figcaption className="mt-1 font-mono text-xs text-muted-foreground">{shot.name}</figcaption>}
                        </figure>
                      ))}
                    </div>
                  )}
                </div>
              )
            }
            case "subagent":
              return (
                <SubagentLink key={item.key} name={item.name} error={item.error} onSubagent={onSubagent}>
                  {item.name} {item.error ? "failed" : "answered"}
                </SubagentLink>
              )
            case "note":
              return item.key === "thinking" ? (
                <ThinkingIndicator key={item.key} />
              ) : (
                <p key={item.key} className={`text-center text-xs ${item.error ? "text-red-600 dark:text-red-400" : "text-muted-foreground"}`}>{item.text}</p>
              )
          }
        })}
        {queued.map(({ text, images }, i) => (
          <div key={`queued.${i}`} className="flex flex-col items-end gap-1">
            {text && <ChatMessage from="user" className="whitespace-pre-wrap opacity-60">{text}</ChatMessage>}
            <p className="text-xs text-muted-foreground">
              Sent{images ? ` with ${images} image${images > 1 ? "s" : ""}` : ""}. Dorf reads it when its commands finish.
            </p>
          </div>
        ))}
      </div>
      <ImageDialog file={opened} onClose={() => setOpened(undefined)} />
    </div>
  )
}

type SubagentLinkProps = { name: string; error?: boolean; onSubagent?: (name: string) => void; className?: string; children: ReactNode }

function SubagentLink({ name, error, onSubagent, className = "text-[13px]", children }: SubagentLinkProps) {
  const Chevron = useIcons()["chevron-right"]
  return (
    <button
      type="button"
      disabled={!onSubagent}
      onClick={() => onSubagent?.(name)}
      className={`flex items-center gap-1 self-start font-mono ${className} ${error ? "text-red-600 dark:text-red-400" : "text-muted-foreground"} enabled:hover:text-foreground`}
    >
      {children}
      {onSubagent && <Chevron className="size-3.5" />}
    </button>
  )
}
