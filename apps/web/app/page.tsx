"use client"

import { useCallback, useEffect, useState } from "react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { InputMessage } from "@/components/ui/input-message"
import { SidebarInset, SidebarProvider } from "@/components/ui/sidebar"
import { SidebarInsetTopbar } from "@/components/sidebar-app/inset-topbar"
import { CommandPalette } from "@/components/dorf/command-palette"
import { ThemeSync } from "@/components/dorf/theme-sync"
import { Picker } from "@/components/dorf/picker"
import { Starting } from "@/components/dorf/starting"
import { Thinking } from "@/components/dorf/thinking"
import { SubagentList, SubagentView } from "@/components/dorf/subagent-panel"
import { ThreadSidebar } from "@/components/dorf/thread-sidebar"
import { Transcript } from "@/components/dorf/transcript"
import { useIcons } from "@/lib/icon-context"
import { api, lastModel, type ModelChoice, modelName, openThread, type Repo, type Thread, useHashId, useThread } from "@/lib/dorf"

const NO_REPO = "none"
const REPO_KEY = "dorf.repo"
const MODEL_KEY = "dorf.model"
const THINKING_KEY = "dorf.thinking"

const stored = (key: string) => {
  try {
    return localStorage.getItem(key)
  } catch {
    return null
  }
}
const store = (key: string, value: string) => {
  try {
    localStorage.setItem(key, value)
  } catch {}
}
// The level last picked, if the model offers it; else the model's default.
const level = (m: ModelChoice | undefined, wanted: string | null) => (wanted && m?.thinking.includes(wanted) ? wanted : m?.defaultThinking)

export default function Page() {
  const id = useHashId()
  const icons = useIcons()
  const [threads, setThreads] = useState<Thread[]>([])
  const [repos, setRepos] = useState<Repo[]>([])
  const [repo, setRepo] = useState(NO_REPO)
  const [draft, setDraft] = useState("")
  const [error, setError] = useState<string>()
  const [starting, setStarting] = useState<{ prompt: string; repo?: string }>()
  const { entries, live, agent, subagents, loadEarlier } = useThread(id)
  const [panel, setPanel] = useState<string>()
  const openSubagent = subagents.find((s) => s.name === panel)
  const [models, setModels] = useState<ModelChoice[]>([])
  const [model, setModel] = useState<string>()
  const [thinking, setThinking] = useState<string>()
  const thread = threads.find((t) => t.id === id)
  const running = Boolean(live.run)

  const refresh = useCallback(() => api<Thread[]>("GET", "/threads").then((list) => setThreads(list.slice().reverse())).catch(() => {}), [])
  useEffect(() => void refresh(), [id, refresh])
  useEffect(() => {
    api<Repo[]>("GET", "/repos")
      .then((list) => {
        setRepos(list)
        let last: string | null = null
        try {
          last = localStorage.getItem(REPO_KEY)
        } catch {}
        if (last && list.some((r) => r.name === last)) setRepo(last)
      })
      .catch(() => {})
  }, [])
  useEffect(() => {
    api<ModelChoice[]>("GET", "/models")
      .then((list) => {
        setModels(list)
        const last = stored(MODEL_KEY)
        setModel(list.some((m) => m.ref === last) ? last! : list[0]?.ref)
        setThinking(stored(THINKING_KEY) ?? undefined)
      })
      .catch(() => {})
  }, [])
  const [shownId, setShownId] = useState(id)
  if (shownId !== id) {
    setShownId(id)
    setError(undefined)
    setPanel(undefined)
  }

  const send = async (value: string) => {
    setDraft("")
    setError(undefined)
    try {
      if (id !== undefined) {
        await api("POST", `/threads/${id}/messages`, { text: value })
        // A message brings an archived Thread back.
        if (thread?.archived) await refresh()
        return
      }
      store(REPO_KEY, repo)
      const chosen = repo !== NO_REPO ? repo : undefined
      setStarting({ prompt: value, repo: chosen })
      const created = await api<Thread>("POST", "/threads", { prompt: value, ...(chosen ? { repo: chosen } : {}), model, thinking: newLevels.length ? newLevel : undefined })
      openThread(created.id)
      await refresh()
    } catch (e) {
      setDraft(value)
      setError((e as Error).message)
    } finally {
      setStarting(undefined)
    }
  }
  const stop = () => id !== undefined && api("POST", `/threads/${id}/abort`).catch((e) => setError(e.message))
  // Archive stops the Thread's work and pauses its machine; it stays open here so it can come straight back.
  const archive = (to: "archive" | "unarchive", thread: number) =>
    api("POST", `/threads/${thread}/${to}`).catch((e) => setError(e.message)).then(refresh)
  const remove = async (thread: number) => {
    await api("DELETE", `/threads/${thread}`).catch((e) => setError(e.message))
    if (thread === id) openThread(undefined)
    await refresh()
  }

  const newModel = models.find((m) => m.ref === model)
  const newLevels = newModel?.thinking ?? []
  const newLevel = newLevels.length ? level(newModel, thinking ?? null) : undefined
  const threadModel = agent?.model && models.find((m) => m.ref === `${agent.model!.provider}/${agent.model!.modelId}`)
  const answered = lastModel(entries)
  const think = (next: string) => id !== undefined && api("POST", `/threads/${id}/thinking`, { level: next }).catch((e) => setError(e.message))

  const controls =
    id === undefined ? (
      <>
        {repos.length > 0 && (
          <Picker
            label="Repository"
            value={repo}
            onChange={setRepo}
            options={[{ value: NO_REPO, label: "No repository" }, ...repos.map((r) => ({ value: r.name, label: r.name }))]}
          />
        )}
        {model && models.length > 1 && (
          <Picker
            label="Model"
            value={model}
            onChange={(v) => (setModel(v), store(MODEL_KEY, v))}
            options={models.map((m) => ({ value: m.ref, label: m.name }))}
          />
        )}
        {newLevel && <Thinking value={newLevel} levels={newLevels} onChange={(v) => (setThinking(v), store(THINKING_KEY, v))} />}
      </>
    ) : (
      <>
        <span className="px-2 text-xs text-muted-foreground">{answered ? modelName(answered) : (threadModel?.name ?? "")}</span>
        {agent && threadModel && threadModel.thinking.length > 0 && (
          <Thinking value={agent.thinkingLevel} levels={threadModel.thinking} onChange={think} />
        )}
      </>
    )

  return (
    <SidebarProvider className="h-dvh">
      <ThreadSidebar threads={threads} current={id} onUnarchive={(t) => void archive("unarchive", t)} onDelete={(t) => void remove(t)} />
      <CommandPalette threads={threads} />
      <ThemeSync />
      <SidebarInset className="max-md:!m-0 max-md:!rounded-none">
        <SidebarInsetTopbar>
          <span className="min-w-0 flex-1 truncate text-sm font-medium">
            {thread ? (thread.repo ? `${thread.repo} · ${thread.title}` : thread.title) : id === undefined ? (starting?.prompt ?? "New Thread") : ""}
          </span>
          {id !== undefined && (
            <>
              <Badge color={running ? "amber" : "gray"} size="compact">{running ? "working" : "idle"}</Badge>
              {thread?.archived ? (
                <Button variant="ghost" size="icon-compact" aria-label="Unarchive Thread" title="Unarchive" onClick={() => void archive("unarchive", id)}>
                  <icons.unarchive />
                </Button>
              ) : (
                <Button variant="ghost" size="icon-compact" aria-label="Archive Thread" title="Archive" onClick={() => void archive("archive", id)}>
                  <icons.archive />
                </Button>
              )}
            </>
          )}
        </SidebarInsetTopbar>

        <div className="relative flex min-h-0 flex-1">
        <div className="flex min-h-0 min-w-0 flex-1 flex-col">
        {starting ? (
          <Starting prompt={starting.prompt} repo={starting.repo} />
        ) : id === undefined ? (
          <div className="flex min-h-0 flex-1 flex-col items-center justify-center gap-2 px-6 text-center">
            <h1 className="text-2xl font-semibold">What should Dorf do?</h1>
            <p className="text-sm text-muted-foreground">Each Thread runs in a Linux machine of its own.</p>
          </div>
        ) : openSubagent ? (
          <SubagentView thread={id} subagent={openSubagent} models={models} onBack={() => setPanel(undefined)} />
        ) : (
          <Transcript entries={entries} live={live} onSubagent={setPanel} loadEarlier={loadEarlier} />
        )}
        {id !== undefined && !starting && <SubagentList subagents={subagents} open={panel} onOpen={setPanel} />}

        {/* A subagent's view is read-only: messages go to the Thread, so the composer waits there. */}
        <div hidden={Boolean(openSubagent)} className="mx-auto w-full max-w-3xl px-3 pb-[max(0.75rem,env(safe-area-inset-bottom))]">
          {error && <p className="px-2 pb-2 text-xs text-red-600 dark:text-red-400">{error}</p>}
          <InputMessage
            value={draft}
            onValueChange={setDraft}
            onSend={(value) => void send(value)}
            placeholder={id === undefined ? "Describe a coding task" : "Ask or tell Dorf anything"}
            status={running ? "streaming" : "idle"}
            onStop={stop}
            leftSlot={controls}
            className="bg-surface-3 shadow-surface-3"
            minRows={id === undefined ? 3 : 1}
            maxRows={6}
            textareaProps={{ className: "max-h-[30dvh]" }}
          />
        </div>
        </div>
        </div>
      </SidebarInset>
    </SidebarProvider>
  )
}
