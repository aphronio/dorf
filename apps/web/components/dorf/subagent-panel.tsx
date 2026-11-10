"use client"

import { useEffect, useState } from "react"
import { Button } from "@/components/ui/button"
import { useIcons } from "@/lib/icon-context"
import { Transcript } from "@/components/dorf/transcript"
import { lastModel, type ModelChoice, modelName, type Subagent, useConversation } from "@/lib/dorf"

// Green while it works, a hollow circle while idle.
const dot = (working: boolean) => `size-1.5 shrink-0 rounded-full ${working ? "animate-pulse bg-green-500" : "border border-muted-foreground/60"}`

// The Thread's subagents over the conversation's right edge, so the conversation keeps its width; the open one is
// marked. On a phone it folds to a count.
export function SubagentList({ subagents, open, onOpen }: { subagents: Subagent[]; open?: string; onOpen: (name: string) => void }) {
  const [unfolded, setUnfolded] = useState(false)
  if (!subagents.length) return null
  return (
    <nav aria-label="Subagents" className={`absolute top-2 right-3 z-20 flex flex-col ${unfolded ? "w-40" : "w-28"} md:w-40 rounded-lg border border-border/60 bg-background/95 p-1 backdrop-blur-sm`}>
      <button
        type="button"
        aria-expanded={unfolded}
        onClick={() => setUnfolded(!unfolded)}
        className="flex items-center justify-between px-2 pt-0.5 pb-1 text-left text-[11px] text-muted-foreground/70 md:pointer-events-none"
      >
        Subagents
        <span className="md:hidden">{subagents.length}</span>
      </button>
      <div className={`${unfolded ? "flex" : "hidden"} -m-0.5 max-h-56 flex-col overflow-y-auto p-0.5 md:flex`}>
        {subagents.map((s) => (
          <button
            key={s.name}
            type="button"
            aria-current={open === s.name}
            onClick={() => (onOpen(s.name), setUnfolded(false))}
            className={`flex min-w-0 shrink-0 items-center gap-2 rounded-md px-2 py-1 text-left text-xs ${open === s.name ? "bg-surface-3 text-foreground" : "text-muted-foreground hover:text-foreground"}`}
          >
            <span className={dot(s.working)} />
            <span className="truncate">{s.name}</span>
          </button>
        ))}
      </div>
    </nav>
  )
}

// A subagent's live conversation in the Thread's place, under a bar that names it, its model and thinking level. The
// arrow or Esc leads back.
export function SubagentView({ thread, subagent, models, onBack }: { thread: number; subagent: Subagent; models: ModelChoice[]; onBack: () => void }) {
  const { entries, live, agent } = useConversation(`/threads/${thread}/subagents/${encodeURIComponent(subagent.name)}/events`)
  const answered = lastModel(entries)
  const ref = subagent.model && `${subagent.model.provider}/${subagent.model.modelId}`
  const model = answered ? modelName(answered) : models.find((m) => m.ref === ref)?.name
  const details = [model, agent?.thinkingLevel !== "off" && agent?.thinkingLevel].filter(Boolean).join(" · ")
  const Back = useIcons()["arrow-left"]
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Escape" || e.defaultPrevented) return
      ;(document.activeElement as HTMLElement | null)?.blur()
      onBack()
    }
    window.addEventListener("keydown", onKey)
    return () => window.removeEventListener("keydown", onKey)
  }, [onBack])
  return (
    <>
      <div className="mx-auto w-full max-w-3xl px-3 pt-2 pr-[8.5rem] md:pr-48">
        <div className="flex items-center gap-2 text-xs text-muted-foreground">
          <Button variant="ghost" size="icon-compact" aria-label="Back to Thread" title="Back to Thread (Esc)" onClick={onBack}>
            <Back />
          </Button>
          <span className={dot(subagent.working)} />
          <span className="min-w-0 truncate font-medium text-foreground">{subagent.name}</span>
          {details && <span className="shrink-0 max-sm:hidden">{details}</span>}
        </div>
      </div>
      <Transcript key={subagent.name} entries={entries} live={live} />
    </>
  )
}
