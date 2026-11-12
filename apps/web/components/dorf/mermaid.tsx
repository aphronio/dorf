"use client"

import { useEffect, useRef, useState } from "react"
import { Code } from "@/components/dorf/code"

// Draws a Mermaid block with Pele, which loads each diagram type on demand. Links and images are off, so a diagram
// loads nothing from elsewhere; its colors are the page's, so it follows the theme without drawing again. Source Pele
// cannot draw shows as code.
export function Mermaid({ code }: { code: string }) {
  const box = useRef<HTMLDivElement>(null)
  const [failed, setFailed] = useState(false)
  useEffect(() => {
    const el = box.current
    if (!el) return
    let live = true
    let destroy: (() => void) | undefined
    setFailed(false)
    import("pele/lazy")
      .then(({ mountAsync }) => mountAsync(el, code, { links: false, images: false, directionBreakpoint: 800, fontSize: 14, onError: () => live && setFailed(true) }))
      .then((m) => {
        if (!live) return m.destroy()
        destroy = () => m.destroy()
        if (m.error) setFailed(true)
      })
      .catch(() => live && setFailed(true))
    return () => {
      live = false
      destroy?.()
    }
  }, [code])
  return (
    <>
      {failed && <Code code={code} lang="mermaid" />}
      <div ref={box} hidden={failed} className="dorf-mermaid relative my-2 overflow-x-auto rounded-md border border-border p-3" />
    </>
  )
}
