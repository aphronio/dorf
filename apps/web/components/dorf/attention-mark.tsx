"use client"

import { useEffect } from "react"
import type { Attention } from "@/lib/attention"

const TITLE = "Dorf"
const SIZE = 64

// The icon with a dot in its corner, red when a Thread failed and green when they all finished.
async function marked(original: string, color: string): Promise<string | undefined> {
  const image = new Image()
  image.src = original
  await image.decode().catch(() => undefined)
  const canvas = document.createElement("canvas")
  canvas.width = canvas.height = SIZE
  const draw = canvas.getContext("2d")
  if (!draw) return undefined
  if (image.naturalWidth) draw.drawImage(image, 0, 0, SIZE, SIZE)
  draw.beginPath()
  draw.arc(SIZE * 0.75, SIZE * 0.25, SIZE * 0.22, 0, 2 * Math.PI)
  draw.fillStyle = color
  draw.fill()
  draw.lineWidth = SIZE * 0.06
  draw.strokeStyle = "#fff"
  draw.stroke()
  return canvas.toDataURL("image/png")
}

// The browser tab says how many Threads need the owner, so a tab in the background still tells them: "(2) Dorf" and
// a dot on the icon. Both go back to plain once none do.
export function AttentionMark({ attention }: { attention: Map<number, Attention> }) {
  const count = attention.size
  const color = [...attention.values()].includes("failed") ? "#ef4444" : count ? "#10b981" : undefined
  // Next streams its own <title> in after the page mounts, so the mark puts its own back whenever the head changes.
  useEffect(() => {
    const title = count ? `(${count}) ${TITLE}` : TITLE
    const keep = () => {
      if (document.title !== title) document.title = title
    }
    keep()
    const watch = new MutationObserver(keep)
    watch.observe(document.head, { subtree: true, childList: true, characterData: true })
    return () => watch.disconnect()
  }, [count])
  useEffect(() => {
    const link = document.querySelector<HTMLLinkElement>("link[rel~='icon']")
    if (!link || !color) return
    const original = link.href
    let gone = false
    void marked(original, color).then((url) => url && !gone && (link.href = url))
    return () => {
      gone = true
      link.href = original
    }
  }, [color])
  return null
}
