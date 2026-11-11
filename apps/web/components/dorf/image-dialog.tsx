"use client"

import { useEffect, useLayoutEffect, useRef, useState } from "react"
import { animate, useReducedMotion } from "motion/react"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog"
import { useObjectUrl } from "@/components/ui/file-thumbnail"
import { useIcons } from "@/lib/icon-context"
import { spring } from "@/lib/springs"

// An image the agent read, as a data URL from the transcript.
export type Picture = { src: string; name: string }
export type Viewed = File | Picture

// Room the panel leaves around the image: its margin, its padding and the title row above.
const ROOM_X = "3.5rem"
const ROOM_Y = "5.75rem"

// Where a zoom started, so the same point of the image stays under it.
type Zoom = { to: boolean; x: number; y: number; fx: number; fy: number; width: number }

// Every image in a Thread, fitted to the window. A click on it, or the button by the title, shows it at full size
// around the point clicked, to scroll through; another click fits it again.
export function ImageDialog({ image, onClose }: { image: Viewed | undefined; onClose: () => void }) {
  const icons = useIcons()
  const reduce = useReducedMotion()
  // The image stays while the dialog closes.
  const [shown, setShown] = useState(image)
  const [zoomed, setZoomed] = useState(false)
  const [natural, setNatural] = useState<{ width: number; height: number }>()
  if (image && image !== shown) {
    setShown(image)
    setZoomed(false)
    setNatural(undefined)
  }
  const file = shown instanceof File ? shown : null
  const objectUrl = useObjectUrl(file)
  const url = file ? objectUrl : (shown as Picture | undefined)?.src
  const scroller = useRef<HTMLDivElement>(null)
  const img = useRef<HTMLImageElement>(null)
  const zoom = useRef<Zoom>(undefined)
  // Only an image bigger than the window has more to show.
  const [larger, setLarger] = useState(false)

  const measure = () => {
    const el = img.current
    if (el?.naturalWidth) setNatural({ width: el.naturalWidth, height: el.naturalHeight })
  }
  // Whether the fitted image shows smaller than it is, once its frame has a size.
  const fit = () => {
    const el = img.current
    if (el && natural && !zoomed) setLarger(natural.width > el.clientWidth + 1 || natural.height > el.clientHeight + 1)
  }
  useLayoutEffect(fit)

  // A click on the image keeps its point in place; the button zooms around the middle of what shows.
  const toggle = (at?: { clientX: number; clientY: number }) => {
    const el = img.current
    if (!el || !larger) return
    const box = el.getBoundingClientRect()
    const view = scroller.current!.getBoundingClientRect()
    const x = at ? at.clientX : view.left + view.width / 2
    const y = at ? at.clientY : view.top + view.height / 2
    const fx = Math.min(1, Math.max(0, (x - box.left) / box.width))
    const fy = Math.min(1, Math.max(0, (y - box.top) / box.height))
    zoom.current = { to: !zoomed, x, y, fx, fy, width: box.width }
    setZoomed(!zoomed)
  }

  // A window that grows or shrinks can leave the image more or less to show.
  useEffect(() => {
    window.addEventListener("resize", fit)
    return () => window.removeEventListener("resize", fit)
  })

  // After the image changes size, scroll so the zoomed point sits where it was, then spring from the old size.
  useLayoutEffect(() => {
    const z = zoom.current
    const el = img.current
    const sc = scroller.current
    zoom.current = undefined
    if (!z || !el || !sc) return
    const view = sc.getBoundingClientRect()
    if (z.to) {
      sc.scrollLeft = z.fx * el.offsetWidth - (z.x - view.left)
      sc.scrollTop = z.fy * el.offsetHeight - (z.y - view.top)
    }
    const box = el.getBoundingClientRect()
    if (reduce) return
    const origin = { x: z.fx * box.width, y: z.fy * box.height }
    el.style.transformOrigin = `${origin.x}px ${origin.y}px`
    animate(
      el,
      { scale: [z.width / box.width, 1], x: [z.x - (box.left + origin.x), 0], y: [z.y - (box.top + origin.y), 0] },
      spring.slow,
    )
  }, [zoomed, reduce])

  const Zoom = zoomed ? icons.minimize : icons.maximize
  // The frame is as wide as the image, as far as the window allows, and as tall as that width makes it: a zoom leaves
  // the panel still, and only an image taller than the window shows bands beside it.
  const width = natural && `min(${natural.width}px, 100vw - ${ROOM_X})`
  const frame = natural && {
    width,
    height: `min(${width} * ${natural.height / natural.width}, 100dvh - ${ROOM_Y})`,
  }
  return (
    <Dialog open={image !== undefined} onOpenChange={(open) => !open && onClose()}>
      <DialogContent size="xl" className="flex w-fit min-w-72 max-w-[calc(100vw-2rem)] flex-col p-3 pt-12">
        <DialogTitle className="absolute left-4 right-24 top-4 truncate text-sm font-medium">{shown?.name}</DialogTitle>
        {larger && (
          <Button variant="ghost" size="icon-sm" className="absolute right-12 top-3" onClick={() => toggle()} aria-label={zoomed ? "Fit to window" : "Show full size"}>
            <Zoom />
          </Button>
        )}
        <div
          ref={scroller}
          style={frame}
          className={`grid grid-cols-[100%] grid-rows-[100%] self-center place-items-center-safe rounded-md ${zoomed ? "overflow-auto" : "overflow-hidden"}`}
        >
          {url && (
            // eslint-disable-next-line @next/next/no-img-element -- an image from the Thread, held by the page
            <img
              ref={img}
              src={url}
              alt={shown?.name ?? ""}
              onLoad={measure}
              onClick={(e) => toggle(e)}
              className={
                zoomed
                  ? "max-w-none cursor-zoom-out"
                  : `max-w-full object-contain ${frame ? "max-h-full" : "max-h-[calc(100dvh-5.75rem)]"} ${larger ? "cursor-zoom-in" : ""}`
              }
            />
          )}
        </div>
      </DialogContent>
    </Dialog>
  )
}
