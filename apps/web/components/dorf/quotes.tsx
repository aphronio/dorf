"use client"

// Passages the owner selects in Dorf's replies, each with an optional comment, kept per Thread until the next message
// takes them along. A reply's element carries its entry id in `data-entry`; a quote names it to lead back there.
import { type RefObject, useCallback, useEffect, useLayoutEffect, useRef, useState } from "react"
import { Button } from "@/components/ui/button"
import { useIcons } from "@/lib/icon-context"

export type Quote = { entry: number; text: string; comment?: string }

const MAX_LENGTH = 8_000
const KEY = (thread: number) => `dorf.quotes.${thread}`

// The quotes waiting for a Thread's next message. They survive a reload, so a half-written review is never lost.
export function useQuotes(thread: number | undefined) {
  const [quotes, setQuotes] = useState<Quote[]>([])
  const [shown, setShown] = useState<number>()
  if (shown !== thread) {
    setShown(thread)
    let saved: Quote[] = []
    try {
      if (thread !== undefined) saved = JSON.parse(localStorage.getItem(KEY(thread)) ?? "[]")
    } catch {}
    setQuotes(Array.isArray(saved) ? saved : [])
  }
  const change = useCallback(
    (next: Quote[]) => {
      setQuotes(next)
      if (thread === undefined) return
      try {
        if (next.length) localStorage.setItem(KEY(thread), JSON.stringify(next))
        else localStorage.removeItem(KEY(thread))
      } catch {}
    },
    [thread],
  )
  return [quotes, change] as const
}

type Picked = { quote: Quote; x: number; y: number; commenting: boolean }

// The selection inside one reply, and where its end is on screen; nothing for a selection anywhere else.
function picked(container: HTMLElement): Omit<Picked, "commenting"> | undefined {
  const selection = window.getSelection()
  if (!selection || selection.isCollapsed || !selection.rangeCount) return
  const range = selection.getRangeAt(0)
  if (!container.contains(range.commonAncestorContainer)) return
  const reply = (node: Node) => (node instanceof Element ? node : node.parentElement)?.closest<HTMLElement>("[data-entry]")
  const source = reply(range.startContainer)
  if (!source || source !== reply(range.endContainer)) return
  const text = selection.toString().trim()
  if (!text) return
  const rects = range.getClientRects()
  const end = rects[rects.length - 1] ?? range.getBoundingClientRect()
  return { quote: { entry: Number(source.dataset.entry), text }, x: end.right, y: end.bottom + 6 }
}

// Selecting text in a reply shows a Comment button below it; the button opens a box for the comment.
export function QuoteSelection({ container, onQuote }: { container: RefObject<HTMLElement | null>; onQuote: (quote: Quote) => void }) {
  const [pick, setPick] = useState<Picked>()
  const commenting = pick?.commenting
  const box = useRef<HTMLDivElement>(null)
  const pressed = useRef(false)

  useEffect(() => {
    const el = container.current
    if (!el || commenting) return
    let frame = 0
    // Waits for the pointer to let go, so the button does not chase a selection still being made.
    const update = () => {
      cancelAnimationFrame(frame)
      frame = requestAnimationFrame(() => {
        if (pressed.current) return
        const found = picked(el)
        setPick(found && { ...found, commenting: false })
      })
    }
    const down = (e: PointerEvent) => {
      if (box.current?.contains(e.target as Node)) return
      pressed.current = true
      setPick(undefined)
    }
    const up = () => {
      pressed.current = false
      update()
    }
    document.addEventListener("selectionchange", update)
    el.addEventListener("pointerdown", down)
    document.addEventListener("pointerup", up)
    // A long press that starts a selection on a touch screen ends in pointercancel.
    document.addEventListener("pointercancel", up)
    el.addEventListener("scroll", update)
    return () => {
      cancelAnimationFrame(frame)
      document.removeEventListener("selectionchange", update)
      el.removeEventListener("pointerdown", down)
      document.removeEventListener("pointerup", up)
      document.removeEventListener("pointercancel", up)
      el.removeEventListener("scroll", update)
    }
  }, [container, commenting])

  // Keeps the button and the box on screen.
  useLayoutEffect(() => {
    const el = box.current
    if (!el || !pick) return
    const { width, height } = el.getBoundingClientRect()
    el.style.left = `${Math.max(8, Math.min(pick.x - width, window.innerWidth - width - 8))}px`
    el.style.top = `${Math.max(8, Math.min(pick.y, window.innerHeight - height - 8))}px`
  }, [pick])

  // The box closes on Escape and on a press outside it.
  useEffect(() => {
    if (!commenting) return
    const outside = (e: PointerEvent) => !box.current?.contains(e.target as Node) && setPick(undefined)
    document.addEventListener("pointerdown", outside)
    return () => document.removeEventListener("pointerdown", outside)
  }, [commenting])

  if (!pick) return null
  const tooLong = pick.quote.text.length > MAX_LENGTH
  const add = (comment: string) => {
    onQuote({ ...pick.quote, ...(comment.trim() ? { comment: comment.trim() } : {}) })
    window.getSelection()?.removeAllRanges()
    setPick(undefined)
  }
  return (
    <div ref={box} className="fixed z-50" style={{ left: pick.x, top: pick.y }}>
      {pick.commenting ? (
        <CommentBox quote={pick.quote.text} onAdd={add} onCancel={() => setPick(undefined)} />
      ) : (
        <Button
          variant="secondary"
          size="compact"
          disabled={tooLong}
          title={tooLong ? "Too long to quote" : undefined}
          // A press would clear the selection before the click lands.
          onPointerDown={(e) => e.preventDefault()}
          onMouseDown={(e) => e.preventDefault()}
          onClick={() => setPick({ ...pick, commenting: true })}
          className="shadow-surface-3"
        >
          Comment
        </Button>
      )}
    </div>
  )
}

function CommentBox({ quote, onAdd, onCancel }: { quote: string; onAdd: (comment: string) => void; onCancel: () => void }) {
  const [comment, setComment] = useState("")
  return (
    <div className="flex w-80 max-w-[calc(100vw-1rem)] flex-col gap-2 rounded-xl bg-surface-3 p-2 shadow-surface-3">
      <p className="line-clamp-2 border-l-2 border-border pl-2 text-xs text-muted-foreground">{quote}</p>
      <textarea
        autoFocus
        rows={2}
        value={comment}
        onChange={(e) => setComment(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Escape") onCancel()
          if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) {
            e.preventDefault()
            onAdd(comment)
          }
        }}
        placeholder="Comment (optional)"
        aria-label="Comment on the quote"
        className="resize-none bg-transparent px-1 text-sm outline-none placeholder:text-muted-foreground"
      />
      <div className="flex justify-end gap-1">
        <Button variant="ghost" size="compact" onClick={onCancel}>Cancel</Button>
        <Button variant="primary" size="compact" onClick={() => onAdd(comment)}>Add</Button>
      </div>
    </div>
  )
}

// The quotes waiting in the composer: one line each, the passage then its comment, wrapping like tags.
export function QuoteChips({ quotes, onChange, disabled }: { quotes: Quote[]; onChange: (quotes: Quote[]) => void; disabled?: boolean }) {
  const X = useIcons().x
  if (!quotes.length) return null
  return (
    <ul className="flex flex-wrap gap-1 pb-1" aria-label="Quotes to send">
      {quotes.map((q, i) => (
        <li key={i} title={q.comment ? `“${q.text}”\n${q.comment}` : `“${q.text}”`} className="flex h-6 max-w-72 items-center rounded-md bg-surface-1 pl-2 text-xs">
          <span className="truncate">
            <span className="text-muted-foreground">“{q.text}”</span>
            {q.comment && <> {q.comment}</>}
          </span>
          <button
            type="button"
            aria-label="Remove quote"
            disabled={disabled}
            onClick={() => onChange(quotes.filter((_, j) => j !== i))}
            className="flex size-6 shrink-0 items-center justify-center text-muted-foreground enabled:hover:text-foreground [&_svg]:size-3"
          >
            <X />
          </button>
        </li>
      ))}
    </ul>
  )
}

// A sent message's quotes, above its text; each leads back to its reply when that reply is loaded.
export function SentQuotes({ quotes, onOpen }: { quotes: Quote[]; onOpen: (entry: number) => void }) {
  return (
    <div className="flex max-w-[85%] flex-col items-end gap-2 self-end">
      {quotes.map((q, i) => (
        <div key={i} className="flex flex-col items-end gap-0.5 text-right">
          <button
            type="button"
            onClick={() => onOpen(q.entry)}
            title="Show in the reply"
            className="line-clamp-3 border-r-2 border-border pr-2 text-left text-xs whitespace-pre-wrap text-muted-foreground hover:text-foreground"
          >
            {q.text}
          </button>
          {q.comment && <p className="pr-2.5 text-sm whitespace-pre-wrap">{q.comment}</p>}
        </div>
      ))}
    </div>
  )
}

// Scrolls to a reply and marks it for a moment.
export function showReply(container: HTMLElement | null, entry: number) {
  const reply = container?.querySelector<HTMLElement>(`[data-entry="${entry}"]`)
  if (!reply) return
  reply.scrollIntoView({ block: "center", behavior: "smooth" })
  reply.animate([{ backgroundColor: "color-mix(in oklab, var(--fluid-accent, #6B97FF) 15%, transparent)" }, { backgroundColor: "transparent" }], { duration: 1600, easing: "ease-out" })
}

export const quoteCount = (n?: number) => (n ? `${n} quote${n > 1 ? "s" : ""}` : "")
