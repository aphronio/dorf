"use client"

import { Button } from "@/components/ui/button"
import { Transcript } from "@/components/dorf/transcript"
import { useIcons } from "@/lib/icon-context"
import { lastModel, type ModelChoice, modelName, useConversation } from "@/lib/dorf"

// One subagent's own conversation, live: beside the Thread on a wide screen, over it on a phone.
export function SubagentPanel({ thread, name, models, onClose }: { thread: number; name: string; models: ModelChoice[]; onClose: () => void }) {
  const { entries, live, agent } = useConversation(`/threads/${thread}/subagents/${encodeURIComponent(name)}/events`)
  const answered = lastModel(entries)
  const model = answered ? modelName(answered) : models.find((m) => agent?.model && m.ref === `${agent.model.provider}/${agent.model.modelId}`)?.name
  const Close = useIcons().x
  return (
    <aside className="fixed inset-0 z-40 flex flex-col bg-background md:static md:z-auto md:w-[min(440px,40vw)] md:border-l md:border-border">
      <div className="flex items-center gap-2 border-b border-border px-4 py-2 pt-[max(0.5rem,env(safe-area-inset-top))]">
        <span className="min-w-0 flex-1 truncate text-sm font-medium">
          Subagent {name}
          {model && <span className="font-normal text-muted-foreground"> · {model}</span>}
        </span>
        <Button variant="ghost" size="icon-compact" aria-label="Close subagent" onClick={onClose}>
          <Close />
        </Button>
      </div>
      <Transcript entries={entries} live={live} />
    </aside>
  )
}
