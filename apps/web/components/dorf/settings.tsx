"use client"

import { useCallback, useEffect, useState } from "react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { api, type Claude, type GitHub } from "@/lib/dorf"

// The deployment's connections. No key reaches this page or the agent: the keys service keeps each one and the broker
// adds it to the agent's requests.
export function Settings() {
  return (
    <div className="min-h-0 flex-1 overflow-y-auto">
      <div className="mx-auto flex w-full max-w-2xl flex-col gap-8 px-6 py-10">
        <h1 className="text-xl font-semibold">Settings</h1>
        <section className="flex flex-col gap-3">
          <h2 className="text-sm font-medium text-muted-foreground">AI subscriptions</h2>
          <ClaudePlan />
        </section>
        <section className="flex flex-col gap-3">
          <h2 className="text-sm font-medium text-muted-foreground">Source control</h2>
          <GitHubAccount />
        </section>
      </div>
    </div>
  )
}

// Claude Code's own `claude setup-token`, run by the keys service: the owner signs in on claude.com and pastes the
// code back. Connecting again, or disconnecting, revokes the token the keys service held.
function ClaudePlan() {
  const [status, setStatus] = useState<Claude | { error: string }>()
  const [url, setUrl] = useState<string>()
  const [code, setCode] = useState("")
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()

  const load = () => api<Claude>("GET", "/claude").then(setStatus, (e: Error) => setStatus({ error: e.message }))
  useEffect(() => void load(), [])

  const step = async (run: () => Promise<void>) => {
    setError(undefined)
    setBusy(true)
    try {
      await run()
    } catch (e) {
      setError((e as Error).message)
      void load()
    } finally {
      setBusy(false)
    }
  }
  const start = () => step(async () => setUrl((await api<{ url: string }>("POST", "/claude/start")).url))
  // A wrong or expired code ends the sign-in, so either way the next try starts from Connect.
  const finish = () =>
    step(async () => {
      setCode("")
      setUrl(undefined)
      setStatus(await api<Claude>("POST", "/claude/code", { code: code.trim() }))
    })
  const disconnect = () =>
    confirm("Disconnect Claude? Its token is revoked, and Threads on the Claude plan stop until you connect again.") &&
    step(async () => setStatus(await api<Claude>("POST", "/claude/disconnect")))

  const ready = status && "connected" in status
  const connected = ready ? status.connected : null
  const day = connected && new Date(connected).toLocaleDateString(undefined, { day: "numeric", month: "short", year: "numeric" })
  return (
    <div className="flex flex-col gap-3 rounded-lg border border-border p-4">
      <div className="flex items-center gap-3">
        <p className="text-sm font-medium">Claude</p>
        {status && (
          <Badge variant="ghost" color={!ready ? "red" : connected ? "green" : "gray"} title={day || undefined}>
            {!ready ? status.error : connected ? `Connected ${day}` : "Not connected"}
          </Badge>
        )}
        <div className="flex-1" />
        {connected && !url && (
          <Button size="compact" variant="ghost" disabled={busy} onClick={() => void disconnect()}>
            Disconnect
          </Button>
        )}
        {ready && (
          <Button
            size="compact"
            variant={url || connected ? "ghost" : "primary"}
            disabled={busy}
            onClick={() => (url ? setUrl(undefined) : void start())}
          >
            {url ? "Cancel" : connected ? "Connect again" : "Connect"}
          </Button>
        )}
      </div>
      {url && (
        <div className="flex flex-col gap-2">
          <Button size="compact" variant="secondary" onClick={() => window.open(url, "_blank", "noopener,noreferrer")}>
            1. Sign in on claude.com
          </Button>
          <form className="flex gap-1.5" onSubmit={(e) => (e.preventDefault(), void finish())}>
            <Input size="compact" placeholder="2. Paste the code claude.com shows" autoComplete="off" value={code} onChange={(e) => setCode(e.target.value)} />
            <Button size="compact" type="submit" disabled={busy || !code.trim()}>
              Finish
            </Button>
          </form>
        </div>
      )}
      {error && <p className="text-xs text-red-600 dark:text-red-400">{error}</p>}
    </div>
  )
}

// Dorf's GitHub App, through GitHub's device flow: the owner approves a short code on github.com, and the keys service
// keeps the sign-in fresh. GitHub shows the app's repositories on its own page.
function GitHubAccount() {
  const [status, setStatus] = useState<GitHub>()
  const [failure, setFailure] = useState<string>()
  const [busy, setBusy] = useState(false)
  // A failed call keeps the last status the page had, so the card stays usable and the error shows beside it.
  const take = (next: GitHub) => (setStatus(next), setFailure(undefined))
  const load = useCallback(
    () => api<GitHub>("GET", "/github").then((next) => (setStatus(next), setFailure(undefined)), (e: Error) => setFailure(e.message)),
    [],
  )
  useEffect(() => void load(), [load])
  const ready = status
  const pending = ready?.pending

  // The keys service waits for GitHub's answer; the page asks it until the code is approved, declined or expired.
  const waiting = pending?.code
  useEffect(() => {
    if (!waiting) return
    const timer = setInterval(() => void load(), 3000)
    return () => clearInterval(timer)
  }, [waiting, load])

  const act = async (path: string) => {
    setBusy(true)
    try {
      take(await api<GitHub>("POST", path))
    } catch (e) {
      setFailure((e as Error).message)
    } finally {
      setBusy(false)
    }
  }
  const open = () => {
    void navigator.clipboard?.writeText(pending!.code).catch(() => {})
    window.open(pending!.url, "_blank", "noopener,noreferrer")
  }
  const disconnect = () =>
    confirm("Disconnect GitHub? Dorf forgets its sign-in, and Threads can no longer push.") && void act("/github/disconnect")

  return (
    <div className="flex flex-col gap-3 rounded-lg border border-border p-4">
      <div className="flex items-center gap-3">
        <p className="text-sm font-medium">GitHub</p>
        {ready && (
          <Badge variant="ghost" color={ready.connected ? "green" : "gray"}>
            {ready.connected ? `Connected as ${ready.login}` : pending ? "Waiting for GitHub" : "Not connected"}
          </Badge>
        )}
        <div className="flex-1" />
        {ready?.connected && ready.repositories && (
          <a className="text-xs text-muted-foreground hover:text-foreground" href={ready.repositories} target="_blank" rel="noopener noreferrer">
            Repositories ↗
          </a>
        )}
        {ready?.connected && (
          <Button size="compact" variant="ghost" disabled={busy} onClick={disconnect}>
            Disconnect
          </Button>
        )}
        {ready && !ready.connected && (
          <Button size="compact" variant={pending ? "ghost" : "primary"} disabled={busy} onClick={() => void act(pending ? "/github/disconnect" : "/github/start")}>
            {pending ? "Cancel" : "Connect"}
          </Button>
        )}
      </div>
      {pending && (
        <div className="flex items-center gap-3">
          <code className="rounded-md bg-muted px-3 py-1.5 font-mono text-lg tracking-widest">{pending.code}</code>
          <Button size="compact" variant="secondary" onClick={open}>
            Copy code and open GitHub
          </Button>
        </div>
      )}
      {(failure ?? ready?.error) && <p className="text-xs text-red-600 dark:text-red-400">{failure ?? ready?.error}</p>}
    </div>
  )
}
