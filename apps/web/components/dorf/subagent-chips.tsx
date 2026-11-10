"use client"

import type { Subagent } from "@/lib/dorf"

// The Thread's subagents at a glance; a click opens one.
export function SubagentChips({ subagents, open, onOpen }: { subagents: Subagent[]; open?: string; onOpen: (name: string) => void }) {
  return (
    <div className="flex min-w-0 shrink gap-1 overflow-x-auto">
      {subagents.map((s) => (
        <button
          key={s.name}
          type="button"
          onClick={() => onOpen(s.name)}
          className={`flex shrink-0 items-center gap-1.5 rounded-md px-2 py-0.5 font-mono text-xs ${open === s.name ? "bg-surface-3 text-foreground" : "text-muted-foreground hover:bg-surface-2"}`}
        >
          <span className={`size-1.5 rounded-full ${s.working ? "animate-pulse bg-[var(--fluid-accent)]" : "bg-muted-foreground/50"}`} />
          {s.name}
        </button>
      ))}
    </div>
  )
}
