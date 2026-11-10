"use client"

import { useState } from "react"
import { useIcons } from "@/lib/icon-context"
import { cn } from "@/lib/utils"

const tail = (s: string, n = 60) => {
  const lines = s.split("\n")
  return lines.length > n ? `… ${lines.length - n} lines above\n${lines.slice(-n).join("\n")}` : s
}

// One line per command; the output opens on click.
export function ToolCall({ command, output, running, error }: { command: string; output?: string; running?: boolean; error?: boolean }) {
  const [open, setOpen] = useState(false)
  const Chevron = useIcons()["chevron-right"]
  return (
    <div className="font-mono text-xs">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        className="flex w-full min-w-0 items-center gap-2 rounded-md px-2 py-1 text-left text-muted-foreground transition-colors duration-80 hover:bg-surface-3 hover:text-foreground"
      >
        <span className={cn("size-1.5 shrink-0 rounded-full", running ? "animate-pulse bg-amber-400" : error ? "bg-red-400" : "bg-emerald-500/70")} />
        <span className="min-w-0 flex-1 truncate">{command.split("\n")[0]}</span>
        <Chevron size={12} className={cn("shrink-0 transition-transform duration-160", open && "rotate-90")} />
      </button>
      {open && (
        <pre className="mt-1 mb-2 ml-5 max-h-96 overflow-auto rounded-md bg-surface-1 px-3 py-2 leading-relaxed whitespace-pre-wrap break-words">
          <span className="text-foreground">$ {command}</span>
          {"\n\n"}
          <span className={error ? "text-red-400" : "text-muted-foreground"}>{output ? tail(output) : running ? "running…" : "(no output)"}</span>
        </pre>
      )}
    </div>
  )
}

type Run = { id: string; command: string; shell: boolean; output?: string; running?: boolean; error?: boolean }

// A run of commands collapses to one line, "Ran 3 shell commands"; a click lists them.
export function ToolCalls({ calls }: { calls: Run[] }) {
  const [open, setOpen] = useState(false)
  const running = calls.find((c) => c.running)
  const failed = calls.filter((c) => c.error).length
  const noun = calls.every((c) => c.shell) ? "shell command" : "tool call"
  return (
    <div className="font-mono text-xs">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        className="flex min-w-0 max-w-full items-center gap-1.5 rounded-md px-2 py-1 text-left text-muted-foreground transition-colors duration-80 hover:bg-surface-3 hover:text-foreground"
      >
        {running && <span className="size-1.5 shrink-0 animate-pulse rounded-full bg-amber-400" />}
        <span className="truncate">
          {running ? "Running" : "Ran"} <b className="font-semibold text-[color:var(--fluid-accent)]">{calls.length}</b> {noun}
          {calls.length === 1 ? "" : "s"}
          {failed > 0 && <span className="text-red-400"> · {failed} failed</span>}
          {running && !open && <span className="text-muted-foreground/70"> · {running.command.split("\n")[0]}</span>}
        </span>
      </button>
      {open && (
        <div className="ml-3 border-l border-border pl-1">
          {calls.map((c) => (
            <ToolCall key={c.id} command={c.command} output={c.output} running={c.running} error={c.error} />
          ))}
        </div>
      )}
    </div>
  )
}
