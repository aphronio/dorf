"use client"

import { useCallback, useEffect, useRef, useState } from "react"

// `starting` until the first message is in, `waiting` meanwhile while no machine is free, since when and with which
// Threads holding the machines, and `failure` if it never got in. `working` while a run is live; `attention` once it ended, until the owner opens the Thread.
// `askedAt` is the owner's last message, `startedAt` and `endedAt` the newest run's, and `ended` how it ended, which
// stays after the owner looked.
export type Thread = {
  id: number
  title: string
  createdAt: string
  repo?: string
  archived?: true
  starting?: true
  waiting?: { since: string; busy: number[] }
  failure?: string
  working?: true
  attention?: "done" | "failed"
  askedAt?: string
  startedAt?: string
  endedAt?: string
  ended?: "done" | "failed" | "stopped"
}

// Names one draft, so sending it again after a lost answer finds the Thread the first send made. Not randomUUID: that
// needs a secure context.
export const newRequest = () => Array.from(crypto.getRandomValues(new Uint8Array(12)), (b) => b.toString(16).padStart(2, "0")).join("")
export type Repo = { name: string; private: boolean }
// When the owner last connected the Claude plan; null until then.
export type Claude = { connected: string | null }
// GitHub as the keys service reports it: who is connected, the code to approve while a sign-in waits, and where the
// owner picks the repositories Dorf's app may reach.
export type GitHub = { connected: string | null; login?: string; pending?: { code: string; url: string }; error?: string; repositories?: string }
export type ModelChoice = { ref: string; name: string; thinking: string[]; defaultThinking: string }
export type ModelRef = { provider: string; modelId: string }
export type ThreadAgent = { model?: ModelRef; thinkingLevel: string }
export type Subagent = { name: string; model?: ModelRef; working: boolean }

export type JsonValue = string | number | boolean | null | JsonValue[] | { [key: string]: JsonValue | undefined }
export type Block =
  | { type: "text"; text: string }
  | { type: "thinking"; thinking: string }
  | { type: "toolCall"; id: string; name: string; arguments?: Record<string, JsonValue> }
  | { type: "image"; data: string; mimeType: string }
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

export class Unreachable extends Error {
  constructor() {
    super("The agent is unreachable")
  }
}

// The agent answers every call in JSON, so anything else came from in front of it: the network, a
// proxy's error page, or the development server's 404 when it proxies to no agent.
export async function api<T>(method: string, path: string, body?: JsonValue): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: { "content-type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  }).catch(() => {
    throw new Unreachable()
  })
  if (res.status === 204) return null as T
  if (!res.headers.get("content-type")?.includes("application/json")) throw new Unreachable()
  const data = await res.json().catch(() => {
    throw new Unreachable()
  })
  if (!res.ok) throw new Error(data?.error ?? `HTTP ${res.status}`)
  return data as T
}

// How long the page waits, at most, before it asks an unreachable agent again.
export const RETRY_MS = 3000

// An event stream that stays open. EventSource retries a dropped connection itself, but gives up for good on an answer
// other than 200, such as a proxy's 502 while the agent restarts; this opens it again after a wait that grows to RETRY_MS.
// `url` is asked again at each reopen, so it can say where to resume; `lost` hears of each answer the browser gave up on.
function stream<E>(url: () => string, on: { event: (event: E) => void; lost?: () => void }): () => void {
  let source: EventSource | undefined
  let timer: ReturnType<typeof setTimeout> | undefined
  let wait = 500
  const open = () => {
    source = new EventSource(url())
    source.onopen = () => void (wait = 500)
    source.onmessage = (m) => on.event(JSON.parse(m.data) as E)
    source.onerror = () => {
      if (source!.readyState !== EventSource.CLOSED) return
      on.lost?.()
      timer = setTimeout(open, wait)
      wait = Math.min(wait * 2, RETRY_MS)
    }
  }
  open()
  return () => {
    clearTimeout(timer)
    source?.close()
  }
}

// The Thread list, newest first, as the agent pushes it: on connect, and whenever a Thread is added, archived, deleted,
// starts working or ends it.
export function useThreads(lost: () => void): Thread[] {
  const [threads, setThreads] = useState<Thread[]>([])
  useEffect(() => stream<{ threads: Thread[] }>(() => "/threads/events", { event: (e) => setThreads(e.threads.slice().reverse()), lost }), [lost])
  return threads
}

export const text = (content: Message["content"] | undefined) =>
  Array.isArray(content)
    ? content.flatMap((c) => (c.type === "text" ? [c.text] : [])).join("")
    : (content ?? "")

// Pi's JSON state stream: the first event holds the newest entries and whether earlier ones exist,
// later ones the new entries. A reconnect sends the newest entry it has and gets the ones it missed, however many, so
// merging by id keeps the transcript whole and in order.
function merge(prev: Entry[], next: Entry[]): Entry[] {
  const byId = new Map(prev.map((e) => [e.id, e]))
  for (const e of next) byId.set(e.id, e)
  return [...byId.values()].sort((a, b) => a.id - b.id)
}

// A message sent while the Thread works, waiting for the current step to end; its images and quotes show once it lands.
export type Queued = { text: string; images: number; quotes?: number }

// A Thread's or a subagent's event stream.
export function useConversation(url: string | undefined) {
  const [entries, setEntries] = useState<Entry[]>([])
  const [live, setLive] = useState<Live>({})
  const [agent, setAgent] = useState<ThreadAgent>()
  const [subagents, setSubagents] = useState<Subagent[]>([])
  const [queued, setQueued] = useState<Queued[]>([])
  const [earlier, setEarlier] = useState(false)
  const following = useRef(url)
  // Another conversation starts empty, set while rendering so its first frame shows none of the last.
  const [shown, setShown] = useState(url)
  if (shown !== url) {
    setShown(url)
    setEntries([])
    setEarlier(false)
    setLive({})
    setAgent(undefined)
    setSubagents([])
    setQueued([])
  }
  useEffect(() => {
    following.current = url
    if (url === undefined) return
    // A reopened stream resumes after the newest entry the page has, as the browser's own reconnect does.
    let newest: number | undefined
    type Event = { entries?: Entry[]; earlier?: boolean; live?: Live; agent?: ThreadAgent; subagents?: Subagent[]; queued?: Queued[] }
    return stream<Event>(() => (newest === undefined ? url : `${url}?after=${newest}`), {
      event: (event) => {
        if (event.entries?.length) {
          newest = Math.max(newest ?? 0, ...event.entries.map((e) => e.id))
          setEntries((prev) => merge(prev, event.entries!))
        }
        if (event.earlier !== undefined) setEarlier(event.earlier)
        if (event.agent) setAgent(event.agent)
        if (event.subagents) setSubagents(event.subagents)
        if (event.live) setLive(event.live)
        if (event.queued) setQueued(event.queued)
      },
    })
  }, [url])
  const loadEarlier = useCallback(async () => {
    const oldest = entries[0]
    if (!url || !oldest) return
    const page = await api<{ entries: Entry[]; earlier: boolean }>("GET", `${url.replace(/\/events$/, "/entries")}?before=${oldest.id}`)
    // The page may have moved to another conversation meanwhile.
    if (following.current !== url) return
    setEntries((prev) => merge(page.entries, prev))
    setEarlier(page.earlier)
  }, [url, entries])
  return { entries, live, agent, subagents, queued, loadEarlier: earlier ? loadEarlier : undefined }
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

export function openThread(id: number | undefined): void {
  window.location.hash = id === undefined ? "" : String(id)
}

// What the page shows: "" for a new Thread, a Thread's id, or "settings".
export function useHash(): string {
  const [hash, setHash] = useState("")
  useEffect(() => {
    const update = () => setHash(window.location.hash.slice(1))
    update()
    window.addEventListener("hashchange", update)
    return () => window.removeEventListener("hashchange", update)
  }, [])
  return hash
}

// ─── Images ──────────────────────────────────────────────────────────────

export const IMAGE_TYPES = "image/png,image/jpeg,image/gif,image/webp"
// The agent takes 8 images in a message and fits each to the model; sending each near this size keeps a message
// from a phone quick, and its whole body well inside the agent's limit.
export const MAX_IMAGES = 8
const MAX_EDGE = 2000
const TARGET_BYTES = 1 << 20

const base64 = (blob: Blob) =>
  new Promise<string>((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => resolve(String(reader.result).slice(String(reader.result).indexOf(",") + 1))
    reader.onerror = () => reject(reader.error)
    reader.readAsDataURL(blob)
  })

// An attached image as the agent takes it: base64 of the file itself when it is small enough, else drawn at most
// 2000 px on its longer edge, as PNG if that is small enough and as JPEG of falling quality if not. The agent
// checks the bytes and fits them to the model either way.
export async function encodeImage(file: File): Promise<string> {
  const bitmap = await createImageBitmap(file).catch(() => {
    throw new Error(`${file.name} is not an image the browser can read`)
  })
  const scale = Math.min(1, MAX_EDGE / Math.max(bitmap.width, bitmap.height))
  if (scale === 1 && file.size <= TARGET_BYTES) {
    bitmap.close()
    return base64(file)
  }
  const canvas = new OffscreenCanvas(Math.round(bitmap.width * scale), Math.round(bitmap.height * scale))
  canvas.getContext("2d")!.drawImage(bitmap, 0, 0, canvas.width, canvas.height)
  bitmap.close()
  let out = await canvas.convertToBlob({ type: "image/png" })
  for (const quality of [0.92, 0.85, 0.77, 0.68]) {
    if (out.size <= TARGET_BYTES) break
    out = await canvas.convertToBlob({ type: "image/jpeg", quality })
  }
  return base64(out)
}

type Image = Extract<Block, { type: "image" }>
const files = new WeakMap<Image, File>()

// A sent image as a File, the shape the chat components show; made once per stored block.
export function imageFile(image: Image, name: string): File {
  let file = files.get(image)
  if (!file) {
    const bytes = Uint8Array.from(atob(image.data), (c) => c.charCodeAt(0))
    file = new File([bytes], name, { type: image.mimeType, lastModified: 0 })
    files.set(image, file)
  }
  return file
}
