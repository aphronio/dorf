"use client"

import { useEffect, useLayoutEffect, useRef } from "react"
import { ChatMessage } from "@/components/ui/chat-message"
import { ThinkingIndicator } from "@/components/ui/thinking-indicator"
import { Markdown } from "@/components/dorf/markdown"
import { ToolCalls } from "@/components/dorf/tool-call"
import { type Block, type Entry, type Live, type Message, text } from "@/lib/dorf"

type Call = Extract<Block, { type: "toolCall" }>
type Output = { text: string; error?: boolean; running?: boolean }
type Item =
  | { kind: "user" | "text"; key: string; text: string }
  | { kind: "calls"; key: string; calls: Call[] }
  | { kind: "note"; key: string; text: string; error?: boolean }

const command = (c: Call) => (c.name === "bash" ? String(c.arguments?.command ?? "") : `${c.name} ${JSON.stringify(c.arguments ?? {})}`)

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

export function Transcript({ entries, live }: { entries: Entry[]; live: Live }) {
  const scroller = useRef<HTMLDivElement>(null)
  const pinned = useRef(true)

  const outputs: Record<string, Output> = {}
  const items: Item[] = []
  for (const e of entries) {
    const m = e.model?.[0]
    if (!m) continue
    if (e.kind === "pi.tool-result" && m.toolCallId) outputs[m.toolCallId] = { text: text(m.content), error: m.isError }
    else if (e.kind === "pi.user") items.push({ kind: "user", key: String(e.id), text: text(m.content) })
    else if (e.kind === "pi.assistant") push(items, String(e.id), m)
    else if (e.kind === "pi.compaction") items.push({ kind: "note", key: String(e.id), text: "Earlier conversation summarized" })
  }
  for (const t of live.tools ?? []) if (t.status !== "done") outputs[t.callId] = { text: t.output ?? "", running: true }
  const generating = live.generation?.message
  if (generating) push(items, "live", generating)
  if (live.run && !text(generating?.content).trim() && !Object.values(outputs).some((o) => o.running))
    items.push({ kind: "note", key: "thinking", text: "" })
  if (live.generation?.retry) items.push({ kind: "note", key: "retry", text: `Retrying: ${live.generation.retry.error}` })
  if (live.runFailure && !live.run)
    items.push({ kind: "note", key: "failed", error: true, text: `Run ended: ${live.runFailure.reason}${live.runFailure.detail ? `, ${live.runFailure.detail}` : ""}` })

  useLayoutEffect(() => {
    const el = scroller.current
    if (el && pinned.current) el.scrollTop = el.scrollHeight
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
        {items.map((item) => {
          switch (item.kind) {
            case "user":
              return <ChatMessage key={item.key} from="user" className="whitespace-pre-wrap">{item.text}</ChatMessage>
            case "text":
              return <ChatMessage key={item.key} from="assistant" className="max-w-full"><Markdown>{item.text}</Markdown></ChatMessage>
            case "calls":
              return (
                <div key={item.key} className="-mx-2">
                  <ToolCalls
                    calls={item.calls.map((c) => ({ id: c.id, command: command(c), shell: c.name === "bash", ...outputs[c.id], output: outputs[c.id]?.text }))}
                  />
                </div>
              )
            case "note":
              return item.key === "thinking" ? (
                <ThinkingIndicator key={item.key} />
              ) : (
                <p key={item.key} className={`text-center text-xs ${item.error ? "text-red-600 dark:text-red-400" : "text-muted-foreground"}`}>{item.text}</p>
              )
          }
        })}
      </div>
    </div>
  )
}
