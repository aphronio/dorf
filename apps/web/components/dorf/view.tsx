"use client"

import { useEffect, useMemo, useRef, useState } from "react"
import { createPortal } from "react-dom"
import { useIcons } from "@/lib/icon-context"

// The theme variables a view may use, copied from the page so it matches light and dark.
const VARS = [
  "background", "foreground", "card", "card-foreground", "muted", "muted-foreground", "border",
  "primary", "primary-foreground", "fluid-accent", "destructive", "surface-1", "surface-2", "surface-3",
] as const

// No network at all: the frame draws from its own bytes.
const CSP = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data:; font-src data:"

// The tallest a view stands in the Thread; past it, the frame scrolls.
const MAX_HEIGHT = 640

// Runs in the frame before the fragment: keeps links from leaving the view, shows the first uncaught script error as
// one line at its top, so a blank view says why, reports its height and takes the page's theme as it changes. A frame
// sized to its content hides its scrollbar, which would narrow the content and change its height again.
const script = (fit: boolean) => `(() => {
  let shown = false
  const fail = (what) => {
    if (shown || !document.body || /^ResizeObserver loop/.test(what)) return
    shown = true
    const line = document.createElement("div")
    line.style.cssText = "padding:6px 12px;font:12px/1.4 ui-monospace,monospace;color:var(--destructive);border-bottom:1px solid var(--border)"
    line.textContent = "Script error: " + what
    document.documentElement.insertBefore(line, document.body)
  }
  addEventListener("error", (e) => e instanceof ErrorEvent && fail(e.message.replace(/^Uncaught /, "")))
  addEventListener("unhandledrejection", (e) => fail(e.reason?.message ?? String(e.reason)))
  addEventListener("click", (e) => e.target.closest?.("a[href]") && e.preventDefault(), true)
  const post = () => {
    const height = Math.ceil(document.documentElement.getBoundingClientRect().height)
    ${fit ? `document.documentElement.style.overflowY = height > ${MAX_HEIGHT} ? "auto" : "hidden"` : ""}
    parent.postMessage({ dorfView: "height", height }, "*")
  }
  new ResizeObserver(post).observe(document.documentElement)
  addEventListener("message", (e) => {
    if (e.source !== parent || !e.data || e.data.dorfView !== "theme") return
    const root = document.documentElement
    for (const [k, v] of Object.entries(e.data.vars)) root.style.setProperty(k, v)
    root.style.colorScheme = e.data.scheme
  })
})()`

type Theme = { vars: Record<string, string>; scheme: string }

function theme(): Theme {
  const style = getComputedStyle(document.documentElement)
  const vars = Object.fromEntries(VARS.map((v) => [`--${v}`, style.getPropertyValue(`--${v}`).trim()]))
  return { vars, scheme: document.documentElement.classList.contains("dark") ? "dark" : "light" }
}

const attr = (s: string) => s.replace(/&/g, "&amp;").replace(/"/g, "&quot;").replace(/</g, "&lt;")

function srcdoc(html: string, t: Theme, fit: boolean) {
  const root = Object.entries(t.vars).map(([k, v]) => `${k}:${v}`).join(";") + `;color-scheme:${t.scheme}`
  return `<!doctype html><html style="${attr(root)}"><head><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="${CSP}">` +
    `<style>html,body{margin:0;background:transparent;color:var(--foreground);font:14px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif}</style>` +
    `<script>${script(fit)}</script></head><body>${html}</body></html>`
}

// One frame of a view. Sandboxed with scripts only, it has an opaque origin: it cannot reach the agent, which
// knows its owner by the network, nor submit forms, open windows, download or navigate the page.
function Frame({ html, fill, title }: { html: string; fill?: boolean; title: string }) {
  const ref = useRef<HTMLIFrameElement>(null)
  const [height, setHeight] = useState(120)
  // A srcdoc loads once. Another load means a script sent the frame elsewhere; the agent's frame-src refused it,
  // leaving an error page.
  const loads = useRef(0)
  const [left, setLeft] = useState(false)
  // The theme at first render; later changes go to the frame as messages, so it keeps its state.
  const doc = useMemo(() => srcdoc(html, theme(), !fill), [html, fill])
  useEffect(() => {
    const onMessage = (e: MessageEvent) => {
      if (e.source !== ref.current?.contentWindow || e.data?.dorfView !== "height") return
      if (typeof e.data.height === "number") setHeight(e.data.height)
    }
    addEventListener("message", onMessage)
    const send = () => ref.current?.contentWindow?.postMessage({ dorfView: "theme", ...theme() }, "*")
    const watch = new MutationObserver(send)
    watch.observe(document.documentElement, { attributes: true, attributeFilter: ["class", "style"] })
    return () => {
      removeEventListener("message", onMessage)
      watch.disconnect()
    }
  }, [])
  if (left) return <p className="px-3 py-2 text-xs text-muted-foreground">This view tried to load another page, and Dorf stopped it.</p>
  return (
    <iframe
      ref={ref}
      onLoad={() => ++loads.current > 1 && setLeft(true)}
      title={title}
      sandbox="allow-scripts"
      srcDoc={doc}
      className="block w-full border-0"
      style={fill ? { height: "100%" } : { height: Math.min(Math.max(height, 24), MAX_HEIGHT) }}
    />
  )
}

// An interactive HTML fragment from a reply's `view` block, shown in the Thread with a button to see it full size.
export function View({ html }: { html: string }) {
  const [full, setFull] = useState(false)
  const { maximize: Maximize, x: Close } = useIcons()
  useEffect(() => {
    if (!full) return
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && setFull(false)
    addEventListener("keydown", onKey)
    return () => removeEventListener("keydown", onKey)
  }, [full])
  return (
    <div className="group relative my-2 overflow-hidden rounded-md border border-border">
      <Frame html={html} title="View" />
      <button
        type="button"
        aria-label="Show full size"
        onClick={() => setFull(true)}
        className="absolute top-1.5 right-1.5 rounded-md border border-border bg-background/90 p-1 text-muted-foreground opacity-0 transition-opacity group-hover:opacity-100 focus-visible:opacity-100 hover:text-foreground"
      >
        <Maximize className="size-4" />
      </button>
      {full &&
        createPortal(
          <div role="dialog" aria-modal="true" aria-label="View" className="fixed inset-0 z-50 flex flex-col bg-background">
            <div className="flex justify-end border-b border-border p-1.5">
              <button type="button" aria-label="Close" onClick={() => setFull(false)} className="rounded-md p-1 text-muted-foreground hover:bg-hover hover:text-foreground">
                <Close className="size-4" />
              </button>
            </div>
            <div className="min-h-0 flex-1">
              <Frame html={html} fill title="View, full size" />
            </div>
          </div>,
          document.body,
        )}
    </div>
  )
}

// A view still streaming in, before its closing fence: shown as a placeholder, so the frame loads once.
export function ViewPending() {
  return <div className="my-2 rounded-md border border-dashed border-border px-3 py-2 text-xs text-muted-foreground">Drawing a view…</div>
}
