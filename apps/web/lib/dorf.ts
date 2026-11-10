"use client"

import { useEffect, useState } from "react"

export type Thread = { id: number; title: string; createdAt: string; repo?: string }
export type Repo = { name: string; private: boolean }
export type Claude = { loggedIn: boolean; plan: string | null; email: string | null }
export type ModelChoice = { ref: string; name: string; thinking: string[]; defaultThinking: string }
export type ModelRef = { provider: string; modelId: string }
export type ThreadAgent = { model?: ModelRef; thinkingLevel: string }
export type Subagent = { name: string; model?: ModelRef; working: boolean }

export type Block =
  | { type: "text"; text: string }
  | { type: "thinking"; thinking: string }
  | { type: "toolCall"; id: string; name: string; arguments?: Record<string, unknown> }
export type Message = {
  role: "user" | "assistant" | "toolResult"
  content: string | Block[]
  model?: string
  responseModel?: string
  stopReason?: string
  errorMessage?: string
  toolCallId?: string
  isError?: boolean
}
export type Entry = { id: number; kind: string; model?: Message[]; data?: unknown }
export type Live = {
  run?: unknown
  generation?: { message?: Message; retry?: { error: string } }
  tools?: { callId: string; status: string; output?: string }[]
  runFailure?: { reason: string; detail?: string }
}

export async function api<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: { "content-type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const data = res.status === 204 ? null : await res.json()
  if (!res.ok) throw new Error(data?.error ?? res.statusText)
  return data as T
}

export const text = (content: Message["content"] | undefined) =>
  typeof content === "string"
    ? content
    : (content ?? []).flatMap((c) => (c.type === "text" ? [c.text] : [])).join("")

// Pi's JSON state stream: the first event holds every entry, later ones the new entries. A
// reconnect starts over, so entries merge by id.
function merge(prev: Entry[], next: Entry[]): Entry[] {
  const byId = new Map(prev.map((e) => [e.id, e]))
  for (const e of next) byId.set(e.id, e)
  return [...byId.values()].sort((a, b) => a.id - b.id)
}

// A Thread's or a subagent's event stream.
export function useConversation(url: string | undefined) {
  const [entries, setEntries] = useState<Entry[]>([])
  const [live, setLive] = useState<Live>({})
  const [agent, setAgent] = useState<ThreadAgent>()
  const [subagents, setSubagents] = useState<Subagent[]>([])
  useEffect(() => {
    setEntries([])
    setLive({})
    setAgent(undefined)
    setSubagents([])
    if (url === undefined) return
    const stream = new EventSource(url)
    stream.onmessage = (m) => {
      const event = JSON.parse(m.data) as { entries?: Entry[]; live?: Live; agent?: ThreadAgent; subagents?: Subagent[] }
      if (event.entries?.length) setEntries((prev) => merge(prev, event.entries!))
      if (event.agent) setAgent(event.agent)
      if (event.subagents) setSubagents(event.subagents)
      setLive(event.live ?? {})
    }
    return () => stream.close()
  }, [url])
  return { entries, live, agent, subagents }
}

export const useThread = (id: number | undefined) => useConversation(id === undefined ? undefined : `/threads/${id}/events`)

// The model that actually answered: "claude-opus-5-5-20261101" reads "Opus 5.5".
export function modelName(id: string): string {
  const claude = id.match(/^claude-([a-z]+)-(\d+)(?:-(\d{1,2}))?(?:-|$)/)
  if (!claude) return id
  return `${claude[1][0].toUpperCase()}${claude[1].slice(1)} ${claude[2]}${claude[3] ? `.${claude[3]}` : ""}`
}

export function lastModel(entries: Entry[]): string | undefined {
  for (let i = entries.length - 1; i >= 0; i--) {
    const m = entries[i].model?.[0]
    if (m?.role === "assistant" && (m.responseModel ?? m.model)) return m.responseModel ?? m.model
  }
}

export function useHashId(): number | undefined {
  const read = () => Number(window.location.hash.slice(1)) || undefined
  const [id, setId] = useState<number | undefined>(undefined)
  useEffect(() => {
    const update = () => setId(read())
    update()
    window.addEventListener("hashchange", update)
    return () => window.removeEventListener("hashchange", update)
  }, [])
  return id
}
