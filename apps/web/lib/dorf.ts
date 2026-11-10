"use client"

import { useEffect, useState } from "react"

export type Thread = { id: number; title: string; createdAt: string; repo?: string }
export type Repo = { name: string; private: boolean }
export type Claude = { loggedIn: boolean; plan: string | null; email: string | null }

export type Block =
  | { type: "text"; text: string }
  | { type: "thinking"; thinking: string }
  | { type: "toolCall"; id: string; name: string; arguments?: Record<string, unknown> }
export type Message = {
  role: "user" | "assistant" | "toolResult"
  content: string | Block[]
  stopReason?: string
  errorMessage?: string
  toolCallId?: string
  isError?: boolean
}
export type Entry = { id: number; kind: string; model?: Message[] }
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

export function useThread(id: number | undefined) {
  const [entries, setEntries] = useState<Entry[]>([])
  const [live, setLive] = useState<Live>({})
  useEffect(() => {
    setEntries([])
    setLive({})
    if (id === undefined) return
    const stream = new EventSource(`/threads/${id}/events`)
    stream.onmessage = (m) => {
      const event = JSON.parse(m.data) as { entries?: Entry[]; live?: Live }
      if (event.entries?.length) setEntries((prev) => merge(prev, event.entries!))
      setLive(event.live ?? {})
    }
    return () => stream.close()
  }, [id])
  return { entries, live }
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
