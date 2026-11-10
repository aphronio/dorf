"use client"

import { useEffect } from "react"
import { useTheme } from "next-themes"
import { Select, SelectContent, SelectItem, SelectTrigger } from "@/components/ui/select"
import { THEMES, type ThemeId, applyTheme, chooseTheme, useThemeId } from "@/lib/themes"

export function ThemePicker() {
  const id = useThemeId()
  const { setTheme } = useTheme()
  useEffect(() => void applyTheme(id, setTheme), [id, setTheme])
  // T and Shift+T step through the themes, unless typing.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key.toLowerCase() !== "t" || e.metaKey || e.ctrlKey || e.altKey || e.repeat) return
      const el = e.target as HTMLElement
      if (el.isContentEditable || ["INPUT", "TEXTAREA", "SELECT"].includes(el.tagName)) return
      const i = THEMES.findIndex((t) => t.id === id)
      chooseTheme(THEMES[(i + (e.shiftKey ? -1 : 1) + THEMES.length) % THEMES.length].id)
    }
    window.addEventListener("keydown", onKey)
    return () => window.removeEventListener("keydown", onKey)
  }, [id, setTheme])
  return (
    <Select value={id} onValueChange={(v) => chooseTheme(v as ThemeId)}>
      <SelectTrigger variant="borderless" className="w-full min-w-0" />
      <SelectContent>
        {THEMES.map((t, i) => (
          <SelectItem key={t.id} value={t.id} index={i}>
            {t.label}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  )
}
