"use client"

import { useEffect, useLayoutEffect, useRef, useState } from "react"
import { Button } from "@/components/ui/button"
import { ChatMessage } from "@/components/ui/chat-message"
import { ThinkingIndicator } from "@/components/ui/thinking-indicator"
import { Markdown } from "@/components/dorf/markdown"
import { ToolCalls } from "@/components/dorf/tool-call"
import { useIcons } from "@/lib/icon-context"
import { type Block, type Entry, type Live, type Message, text } from "@/lib/dorf"

type Call = Extract<Block, { type: "toolCall" }>
type Output = { text: string; error?: boolean; running?: boolean; images?: string[] }
type Item =
  | { kind: "user" | "text"; key: string; text: string; images?: Map<string, string> }
  | { kind: "calls"; key: string; calls: Call[] }
  | { kind: "note"; key: string; text: string; error?: boolean }
  | { kind: "subagent"; key: string; name: string; text: string; error?: boolean }

const command = (c: Call) => {
  const a = c.arguments ?? {}
  if (c.name === "bash") return String(a.command ?? "")
  if (c.name === "subagent") return [`subagent ${a.action}`, a.name, a.message && `: ${a.message}`].filter(Boolean).join(" ")
  return `${c.name} ${JSON.stringify(a)}`
}


// Consecutive commands form one block, even across the assistant messages Pi splits them into.
function push(items: Item[], key: string, message: Message) {
  const blocks = typeof message.content === "string" ? [{ type: "text", text: message.content } as Block] : message.content
  blocks.forEach((b, i) => {
    const last = items.at(-1)
    if (b.type === "toolCall") {
      if (last?.kind === "calls") last.calls.push(b)
      else items.push({ kind: "calls", key: `${key}.${i}`, calls: [b] })
    } else if (b.type === "text" && b.text.trim()) items.push({ kind: "text", key: `${key}.${i}`, text: b.text })
  })
  if (message.stopReason === "error") items.push({ kind: "note", key: `${key}.e`, text: message.errorMessage ?? "The model failed.", error: true })
}

type TranscriptProps = { entries: Entry[]; live: Live; queued?: string[]; onSubagent?: (name: string) => void; loadEarlier?: () => Promise<void> }

export function Transcript({ entries, live, queued = [], onSubagent, loadEarlier }: TranscriptProps) {
  const Chevron = useIcons()["chevron-right"]
  const scroller = useRef<HTMLDivElement>(null)
  const pinned = useRef(true)
  const [loading, setLoading] = useState(false)
  const [loadFailed, setLoadFailed] = useState(false)
  // Set while earlier entries load: the distance from the bottom to keep once they land above.
  const fromBottom = useRef<number>(undefined)
  const oldest = useRef<number>(undefined)

  const outputs: Record<string, Output> = {}
  const items: Item[] = []
  for (const e of entries) {
    const m = e.model?.[0]
    if (!m) continue
    if (e.kind === "pi.tool-result" && m.toolCallId) {
      const images = typeof m.content === "string" ? [] : m.content.flatMap((b) => (b.type === "image" ? [`data:${b.mimeType};base64,${b.data}`] : []))
      outputs[m.toolCallId] = { text: text(m.content), error: m.isError, images }
    }
    else if (e.kind === "pi.user") items.push({ kind: "user", key: String(e.id), text: text(m.content) })
    else if (e.kind === "dorf.subagent-report") {
      // The model reads "[subagent name answered…] text"; the page shows the author once.
      const { name, failed } = e.data as { name: string; failed?: true }
      const body = text(m.content)
      items.push({ kind: "subagent", key: String(e.id), name, text: failed ? body : body.slice(body.indexOf("] ") + 2), error: failed })
    }
    else if (e.kind === "pi.assistant") push(items, String(e.id), m)
    else if (e.kind === "pi.compaction") items.push({ kind: "note", key: String(e.id), text: "Earlier conversation summarized" })
  }
  for (const t of live.tools ?? []) if (t.status !== "done") outputs[t.callId] = { text: t.output ?? "", running: true }
  const generating = live.generation?.message
  if (generating) push(items, "live", generating)
  // A reply places the images read before it, by path and by file name; a later read of the same name leaves it as it was.
  let images = new Map<string, string>()
  for (const item of items) {
    if (item.kind === "text") item.images = images
    if (item.kind !== "calls") continue
    for (const c of item.calls) {
      const src = outputs[c.id]?.images?.[0]
      const path = String(c.arguments?.path ?? "")
      if (src && path) images = new Map(images).set(path, src).set(path.split("/").pop()!, src)
    }
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
        {loadEarlier && (
          <Button
            variant="ghost"
            size="compact"
            className={`self-center ${loadFailed ? "text-red-600 dark:text-red-400" : ""}`}
            loading={loading}
            onClick={async () => {
              const el = scroller.current!
              fromBottom.current = el.scrollHeight - el.scrollTop
              setLoading(true)
              setLoadFailed(false)
              await loadEarlier().catch(() => {
                fromBottom.current = undefined
                setLoadFailed(true)
              })
              setLoading(false)
            }}
          >
            {loadFailed ? "Couldn't load earlier. Retry" : "Load earlier"}
          </Button>
        )}
        {items.map((item) => {
          switch (item.kind) {
            case "user":
              return <ChatMessage key={item.key} from="user" className="whitespace-pre-wrap">{item.text}</ChatMessage>
            case "text":
              // A reply spans the column, so a view in it does too, however short its text.
              return <ChatMessage key={item.key} from="assistant" className="max-w-full self-stretch items-stretch"><Markdown images={item.images}>{item.text}</Markdown></ChatMessage>
            case "calls": {
              // Images a tool returned, such as screenshots the agent read, show small here; a reply places them full size.
              const shots = item.calls.flatMap((c) => (outputs[c.id]?.images ?? []).map((src, i) => ({ src, key: `${c.id}.${i}`, name: String(c.arguments?.path ?? "").split("/").pop() })))
              return (
                <div key={item.key} className="-mx-2">
                  <ToolCalls
                    calls={item.calls.map((c) => ({ id: c.id, command: command(c), shell: c.name === "bash", ...outputs[c.id], output: outputs[c.id]?.text }))}
                  />
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
                <button
                  key={item.key}
                  type="button"
                  disabled={!onSubagent}
                  onClick={() => onSubagent?.(item.name)}
                  className={`flex items-center gap-1 self-start font-mono text-[13px] ${item.error ? "text-red-600 dark:text-red-400" : "text-muted-foreground"} enabled:hover:text-foreground`}
                >
                  {item.name} {item.error ? "failed" : "answered"}
                  {onSubagent && <Chevron className="size-3.5" />}
                </button>
              )
            case "note":
              return item.key === "thinking" ? (
                <ThinkingIndicator key={item.key} />
              ) : (
                <p key={item.key} className={`text-center text-xs ${item.error ? "text-red-600 dark:text-red-400" : "text-muted-foreground"}`}>{item.text}</p>
              )
          }
        })}
        {queued.map((text, i) => (
          <div key={`queued.${i}`} className="flex flex-col items-end gap-1">
            <ChatMessage from="user" className="whitespace-pre-wrap opacity-60">{text}</ChatMessage>
            <p className="text-xs text-muted-foreground">Sent. Dorf reads it when the current step ends.</p>
          </div>
        ))}
      </div>
    </div>
  )
}
