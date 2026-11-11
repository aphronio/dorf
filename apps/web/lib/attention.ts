"use client"

import { useState } from "react"
import type { Thread } from "@/lib/dorf"

export type Attention = NonNullable<Thread["attention"]>

// The Threads that need the owner, with what each asks for: those whose run ended since the owner last looked. The
// Thread the owner opens counts as it did on opening until the owner leaves it, so its row doesn't move while it is
// read and a run ending meanwhile doesn't add it. It counts as opened once it is in the list, so a page loaded
// straight onto it holds it too. The sidebar and the browser tab both count from this one map.
export function useAttention(threads: Thread[], current: number | undefined): Map<number, Attention> {
  const [opened, setOpened] = useState<number>()
  const [held, setHeld] = useState<Attention>()
  const open = threads.find((t) => t.id === current)
  if (opened !== current && (current === undefined || open)) {
    setOpened(current)
    setHeld(open?.attention)
  }
  const attention = new Map<number, Attention>()
  for (const t of threads) {
    const asks = t.id === opened ? held : t.id === current ? undefined : t.attention
    if (asks && !t.archived) attention.set(t.id, asks)
  }
  return attention
}
