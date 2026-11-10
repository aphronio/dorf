"use client"

import { useEffect } from "react"
import { useTheme } from "next-themes"
import { applyTheme, useThemeId } from "@/lib/themes"

// Applies the theme on screen. Choosing one is in ⌘⇧P.
export function ThemeSync() {
  const id = useThemeId()
  const { setTheme } = useTheme()
  useEffect(() => void applyTheme(id, setTheme), [id, setTheme])
  return null
}
