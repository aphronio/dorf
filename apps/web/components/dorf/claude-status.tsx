"use client"

import { useEffect, useState } from "react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { api, type Claude } from "@/lib/dorf"

// Claude Code's own sign-in: the page shows its URL and passes back the code; Dorf never holds the login.
export function ClaudeStatus() {
  const [status, setStatus] = useState<Claude>()
  const [url, setUrl] = useState<string>()
  const [code, setCode] = useState("")
  const [error, setError] = useState<string>()

  const load = () => api<Claude>("GET", "/claude").then(setStatus).catch(() => setStatus({ loggedIn: false, plan: null, email: null }))
  useEffect(() => void load(), [])

  if (!status) return null
  if (status.loggedIn)
    return (
      <div className="flex min-w-0 items-center gap-2 px-2 py-1.5 text-xs text-muted-foreground">
        <Badge color="gray" size="compact">Claude</Badge>
        <span className="truncate">{[status.email, status.plan && `${status.plan} plan`].filter(Boolean).join(" · ")}</span>
      </div>
    )

  const start = async () => {
    setError(undefined)
    try {
      setUrl((await api<{ url: string }>("POST", "/claude/login")).url)
    } catch (e) {
      setError((e as Error).message)
    }
  }
  const finish = async () => {
    setError(undefined)
    try {
      await api("POST", "/claude/code", { code: code.trim() })
      setUrl(undefined)
      await load()
    } catch (e) {
      setError((e as Error).message)
    }
  }

  return (
    <div className="flex flex-col gap-2 px-2 py-1.5">
      {url ? (
        <>
          <Button size="compact" onClick={() => window.open(url, "_blank", "noopener,noreferrer")}>1. Sign in with Claude</Button>
          <form className="flex gap-1.5" onSubmit={(e) => (e.preventDefault(), void finish())}>
            <Input size="compact" placeholder="2. Paste the code" autoComplete="off" value={code} onChange={(e) => setCode(e.target.value)} />
            <Button size="compact" variant="secondary" type="submit" disabled={!code.trim()}>Finish</Button>
          </form>
        </>
      ) : (
        <Button size="compact" onClick={start}>Connect Claude</Button>
      )}
      {error && <p className="text-xs text-red-600 dark:text-red-400">{error}</p>}
    </div>
  )
}
