"use client"

import { useEffect, useState } from "react"

// Time since `since`, as 0:42 or 1:02:05, counting up each second on the page's own clock.
export function Elapsed({ since }: { since: string }) {
  const [now, setNow] = useState(Date.now)
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(timer)
  }, [])
  const s = Math.max(0, Math.floor((now - Date.parse(since)) / 1000))
  const [h, m] = [Math.floor(s / 3600), Math.floor((s % 3600) / 60)]
  const two = (n: number) => String(n).padStart(2, "0")
  return <span className="tabular-nums">{h ? `${h}:${two(m)}:${two(s % 60)}` : `${m}:${two(s % 60)}`}</span>
}
