"use client"

import { type PointerEvent, useEffect, useLayoutEffect, useRef, useState, useSyncExternalStore } from "react"
import { flushSync } from "react-dom"
import { animate, type AnimationPlaybackControls, useReducedMotion } from "motion/react"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog"
import { useObjectUrl } from "@/components/ui/file-thumbnail"
import { useIcons } from "@/lib/icon-context"
import { spring } from "@/lib/springs"

// An image the agent read, as a data URL from the transcript.
export type Picture = { src: string; name: string }
export type Viewed = File | Picture

// Room the panel leaves around the image: its margin, its padding and the title row above.
const ROOM_X = 56
const ROOM_Y = 92
// The closest a zoom goes: each pixel of the image four pixels wide.
const MAX = 4
// How far a pointer moves before a press drags the image instead of clicking it.
const DRAG = 4

// The window's size, which the frame and the fitted size follow.
const subscribe = (change: () => void) => {
  window.addEventListener("resize", change)
  return () => window.removeEventListener("resize", change)
}
const windowSize = () => `${window.innerWidth}x${window.innerHeight}`

// The width a scroll bar takes, zero where scroll bars float over the content.
let bar: number | undefined
function scrollBar() {
  if (bar === undefined) {
    const probe = document.createElement("div")
    probe.style.cssText = "position:absolute;visibility:hidden;width:100px;height:100px;overflow:scroll"
    document.body.append(probe)
    bar = probe.offsetWidth - probe.clientWidth
    probe.remove()
  }
  return bar
}

// Where a zoom started, so the same point of the image stays under it.
type Zoom = { x: number; y: number; fx: number; fy: number; width: number; spring: boolean }

// Every image in a Thread, fitted to the window. The scroll wheel, a pinch on a trackpad or a touch screen zoom in and
// out around the pointer or between the fingers; a drag moves the zoomed image. A click, or the button by the title,
// switches between the fitted image and the image at full size.
export function ImageDialog({ image, onClose }: { image: Viewed | undefined; onClose: () => void }) {
  const icons = useIcons()
  const reduce = useReducedMotion()
  // The image stays while the dialog closes.
  const [shown, setShown] = useState(image)
  const [natural, setNatural] = useState<{ width: number; height: number }>()
  // Image pixels to screen pixels; none while the image is fitted.
  const [scale, setScale] = useState<number>()
  if (image && image !== shown) {
    setShown(image)
    setNatural(undefined)
    setScale(undefined)
  }
  const file = shown instanceof File ? shown : null
  const objectUrl = useObjectUrl(file)
  const url = file ? objectUrl : (shown as Picture | undefined)?.src
  const scroller = useRef<HTMLDivElement>(null)
  // The frame mounts with the dialog's panel, which can come a render after the dialog opens.
  const [frameEl, setFrameEl] = useState<HTMLDivElement | null>(null)
  const img = useRef<HTMLImageElement>(null)
  const zoom = useRef<Zoom>(undefined)
  const motion = useRef<AnimationPlaybackControls>(undefined)

  // The frame is as wide as the image, as far as the window allows, and as tall as that width makes it, so a zoom
  // leaves the panel still. An image taller than the window gets room beside it for the scroll bar.
  const size = useSyncExternalStore(subscribe, windowSize, () => "0x0")
  const [viewW, viewH] = size.split("x").map(Number)
  let frame: { width: number; height: number } | undefined
  let fit = 1
  if (natural && viewW) {
    const room = { width: viewW - ROOM_X, height: viewH - ROOM_Y }
    let width = Math.min(natural.width, room.width)
    let height = (width * natural.height) / natural.width
    if (height > room.height) {
      height = room.height
      width = Math.min(natural.width + scrollBar(), room.width)
    }
    frame = { width, height }
    fit = Math.min(1, width / natural.width, height / natural.height)
  }
  const now = scale ?? fit
  const zoomed = now > fit
  // A ref, so handlers that run between renders read the scale the last zoom set.
  const current = useRef(now)
  useLayoutEffect(() => {
    current.current = now
  })

  // Zooms to `next` around a point on screen, keeping the point of the image under it.
  const zoomTo = (next: number | undefined, x: number, y: number, withSpring: boolean) => {
    const el = img.current
    if (!el || !natural) return
    motion.current?.complete()
    const to = Math.min(MAX, Math.max(fit, next ?? fit))
    if (Math.abs(to - current.current) < 1e-3) return
    const box = el.getBoundingClientRect()
    const fx = Math.min(1, Math.max(0, (x - box.left) / box.width))
    const fy = Math.min(1, Math.max(0, (y - box.top) / box.height))
    zoom.current = { x, y, fx, fy, width: box.width, spring: withSpring }
    // A gesture sends many events a frame; each one lays the image out before the next measures it.
    const set = () => setScale(to > fit ? to : undefined)
    if (withSpring) set()
    else flushSync(set)
  }
  const middle = () => {
    const view = scroller.current!.getBoundingClientRect()
    return [view.left + view.width / 2, view.top + view.height / 2] as const
  }
  // The switch: full size from the fitted image, the fitted image from any zoom.
  const toggle = (x: number, y: number) => zoomTo(zoomed ? fit : Math.max(1, fit), x, y, true)

  // After the image changes size, scroll so the zoomed point sits where it was; a click springs from the old size.
  useLayoutEffect(() => {
    const z = zoom.current
    const el = img.current
    const sc = scroller.current
    zoom.current = undefined
    if (!z || !el || !sc) return
    const view = sc.getBoundingClientRect()
    sc.scrollLeft = z.fx * el.offsetWidth - (z.x - view.left)
    sc.scrollTop = z.fy * el.offsetHeight - (z.y - view.top)
    if (!z.spring || reduce) return
    const box = el.getBoundingClientRect()
    const origin = { x: z.fx * box.width, y: z.fy * box.height }
    el.style.transformOrigin = `${origin.x}px ${origin.y}px`
    motion.current = animate(
      el,
      { scale: [z.width / box.width, 1], x: [z.x - (box.left + origin.x), 0], y: [z.y - (box.top + origin.y), 0] },
      spring.slow,
    )
  }, [scale, reduce])

  // The wheel and a trackpad pinch, which browsers send as the wheel with Ctrl held. Safari sends its own gesture
  // events for a pinch, and touch screens send two touches. Each listener takes the event from the browser, which
  // would otherwise scroll or zoom the page.
  const zoomRef = useRef(zoomTo)
  useLayoutEffect(() => {
    zoomRef.current = zoomTo
  })
  useEffect(() => {
    const sc = frameEl
    if (!sc) return
    const wheel = (e: WheelEvent) => {
      e.preventDefault()
      const lines = e.deltaMode === 1 ? 16 : e.deltaMode === 2 ? 400 : 1
      // A pinch moves in small steps; a notch of the wheel is about 100.
      const step = (e.ctrlKey ? 0.01 : 0.002) * e.deltaY * lines
      zoomRef.current(current.current * Math.exp(-step), e.clientX, e.clientY, false)
    }
    let gestureStart = 1
    const gestureBegin = (e: Event) => {
      e.preventDefault()
      gestureStart = current.current
    }
    const gesture = (e: Event) => {
      e.preventDefault()
      const g = e as Event & { scale: number; clientX: number; clientY: number }
      zoomRef.current(gestureStart * g.scale, g.clientX, g.clientY, false)
    }
    let pinch: { distance: number; scale: number } | undefined
    const apart = (t: TouchList) => Math.hypot(t[0].clientX - t[1].clientX, t[0].clientY - t[1].clientY)
    const touchStart = (e: TouchEvent) => {
      if (e.touches.length === 2) pinch = { distance: apart(e.touches), scale: current.current }
    }
    const touchMove = (e: TouchEvent) => {
      if (e.touches.length !== 2 || !pinch) return
      e.preventDefault()
      const [a, b] = [e.touches[0], e.touches[1]]
      zoomRef.current((pinch.scale * apart(e.touches)) / pinch.distance, (a.clientX + b.clientX) / 2, (a.clientY + b.clientY) / 2, false)
    }
    const touchEnd = (e: TouchEvent) => {
      if (e.touches.length < 2) pinch = undefined
    }
    sc.addEventListener("wheel", wheel, { passive: false })
    sc.addEventListener("gesturestart", gestureBegin)
    sc.addEventListener("gesturechange", gesture)
    sc.addEventListener("touchstart", touchStart, { passive: true })
    sc.addEventListener("touchmove", touchMove, { passive: false })
    sc.addEventListener("touchend", touchEnd)
    sc.addEventListener("touchcancel", touchEnd)
    return () => {
      sc.removeEventListener("wheel", wheel)
      sc.removeEventListener("gesturestart", gestureBegin)
      sc.removeEventListener("gesturechange", gesture)
      sc.removeEventListener("touchstart", touchStart)
      sc.removeEventListener("touchmove", touchMove)
      sc.removeEventListener("touchend", touchEnd)
      sc.removeEventListener("touchcancel", touchEnd)
    }
  }, [frameEl])

  // A mouse drags a zoomed image; a press that barely moves is a click. Touch scrolls the frame as usual, and a tap,
  // which the browser sends as a click, switches.
  const drag = useRef<{ x: number; y: number; left: number; top: number; moved: boolean }>(undefined)
  const [dragging, setDragging] = useState(false)
  const mouse = useRef(false)
  const press = (e: PointerEvent<HTMLImageElement>) => {
    mouse.current = e.pointerType === "mouse"
    if (!mouse.current || e.button !== 0) return
    e.preventDefault()
    const sc = scroller.current!
    drag.current = { x: e.clientX, y: e.clientY, left: sc.scrollLeft, top: sc.scrollTop, moved: false }
  }
  const move = (e: PointerEvent<HTMLImageElement>) => {
    const d = drag.current
    if (!d) return
    if (!d.moved && Math.hypot(e.clientX - d.x, e.clientY - d.y) < DRAG) return
    if (!d.moved) {
      d.moved = true
      e.currentTarget.setPointerCapture(e.pointerId)
      setDragging(true)
    }
    const sc = scroller.current!
    sc.scrollLeft = d.left - (e.clientX - d.x)
    sc.scrollTop = d.top - (e.clientY - d.y)
  }
  const release = (e: PointerEvent<HTMLImageElement>) => {
    const d = drag.current
    drag.current = undefined
    setDragging(false)
    if (d && !d.moved) toggle(e.clientX, e.clientY)
  }

  const canZoom = zoomed || fit < 1
  const Zoom = zoomed ? icons.minimize : icons.maximize
  return (
    <Dialog open={image !== undefined} onOpenChange={(open) => !open && onClose()}>
      <DialogContent size="xl" className="flex w-fit min-w-72 max-w-[calc(100vw-2rem)] flex-col p-3 pt-12">
        <DialogTitle className="absolute left-4 right-24 top-4 truncate text-sm font-medium">{shown?.name}</DialogTitle>
        {canZoom && (
          <Button variant="ghost" size="icon-sm" className="absolute right-12 top-3" onClick={() => toggle(...middle())} aria-label={zoomed ? "Fit to window" : "Show full size"}>
            <Zoom />
          </Button>
        )}
        <div
          ref={(el) => {
            scroller.current = el
            setFrameEl(el)
          }}
          style={frame}
          className={`grid grid-cols-[100%] grid-rows-[100%] self-center place-items-center-safe overscroll-contain rounded-md touch-pan-x touch-pan-y ${zoomed ? "overflow-auto" : "overflow-hidden"}`}
        >
          {url && (
            // oxlint-disable-next-line nextjs/no-img-element, jsx-a11y/click-events-have-key-events, jsx-a11y/no-noninteractive-element-interactions -- an image from the Thread, held by the page; a click zooms it like a pinch does
            <img
              ref={img}
              src={url}
              alt={shown?.name ?? ""}
              draggable={false}
              onLoad={(e) => setNatural({ width: e.currentTarget.naturalWidth, height: e.currentTarget.naturalHeight })}
              onPointerDown={press}
              onPointerMove={move}
              onPointerUp={release}
              onClick={(e) => !mouse.current && toggle(e.clientX, e.clientY)}
              onPointerCancel={() => {
                drag.current = undefined
                setDragging(false)
              }}
              style={natural ? { width: natural.width * now, height: natural.height * now } : undefined}
              className={`select-none ${natural ? "max-w-none" : "max-h-[calc(100dvh-5.75rem)] max-w-full"} ${
                dragging ? "cursor-grabbing" : zoomed ? "cursor-zoom-out" : canZoom ? "cursor-zoom-in" : ""
              }`}
            />
          )}
        </div>
      </DialogContent>
    </Dialog>
  )
}
