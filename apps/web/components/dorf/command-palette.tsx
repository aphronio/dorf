"use client"

import { useEffect, useMemo, useState } from "react"
import { CommandMenu, CommandMenuDialog, CommandMenuInput, CommandMenuList, type CommandMenuItemData } from "@/components/ui/command-menu"
import { ThemeSample } from "@/components/dorf/theme-sample"
import { THEMES, type ThemeId, chooseTheme, previewTheme, savedTheme } from "@/lib/themes"
import type { Thread } from "@/lib/dorf"

type Mode = "threads" | "commands" | "theme"

const PLACEHOLDER: Record<Mode, string> = {
  threads: "Search Threads (type > for commands)",
  commands: "Type a command",
  theme: "Select a theme (↑↓ to preview)",
}

// Like VS Code: ⌘K searches Threads as ⌘P searches files, ⌘⇧P runs commands, and ">" in the Thread
// search switches to commands. "Change theme" previews each theme as it is highlighted; Enter keeps
// it, Escape goes back to the saved one.
export function CommandPalette({ threads }: { threads: Thread[] }) {
  const [mode, setMode] = useState<Mode | null>(null)
  const [query, setQuery] = useState("")

  const show = (next: Mode) => {
    setMode(next)
    setQuery("")
  }
  const close = () => {
    previewTheme(null)
    setMode(null)
    setQuery("")
  }

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (!(e.metaKey || e.ctrlKey) || e.altKey) return
      const key = e.key.toLowerCase()
      if (key === "k" && !e.shiftKey) show("threads")
      else if (key === "p" && e.shiftKey) show("commands")
      else return
      e.preventDefault()
    }
    window.addEventListener("keydown", onKey)
    return () => window.removeEventListener("keydown", onKey)
  }, [])

  const items = useMemo<CommandMenuItemData[]>(() => {
    if (mode === "theme") return THEMES.map((t) => ({ value: t.id, label: t.label, group: "Themes", action: "Keep theme" }))
    if (mode === "commands")
      return [
        { value: "new", label: "New Thread", group: "Commands", action: "Start" },
        { value: "theme", label: "Change theme…", group: "Commands", action: "Choose" },
      ]
    return threads.map((t) => ({
      value: `thread:${t.id}`,
      label: t.title,
      description: t.repo,
      keywords: t.repo ? [t.repo] : [],
      group: "Threads",
      action: "Open",
    }))
  }, [mode, threads])

  const select = (item: CommandMenuItemData) => {
    if (mode === "theme") {
      chooseTheme(item.value as ThemeId)
      return close()
    }
    if (item.value === "theme") return show("theme")
    window.location.hash = item.value === "new" ? "" : item.value.slice("thread:".length)
    close()
  }

  const onQuery = (q: string) => {
    if (mode === "threads" && q.startsWith(">")) {
      setMode("commands")
      return setQuery(q.slice(1).trimStart())
    }
    setQuery(q)
  }

  return (
    <CommandMenuDialog open={mode !== null} onOpenChange={(o) => !o && close()} shortcut={null} title="Command palette">
      <CommandMenu
        items={items}
        query={query}
        onQueryChange={onQuery}
        onSelect={select}
        onActiveChange={(item) => mode === "theme" && previewTheme((item?.value as ThemeId) ?? null)}
        suggestions={mode === "theme" ? [savedTheme()] : undefined}
        suggestionsLabel="Current"
        closeOnSelect={false}
      >
        <CommandMenuInput placeholder={PLACEHOLDER[mode ?? "threads"]} />
        <CommandMenuList className={mode === "theme" ? "max-h-[28vh]" : undefined} />
        {mode === "theme" && <ThemeSample />}
      </CommandMenu>
    </CommandMenuDialog>
  )
}
