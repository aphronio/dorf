"use client"

import { useSyncExternalStore } from "react"

// Famous editor themes, as Shiki ships them. The page's colors derive from each theme's own editor
// colors; `accent` names the key that carries its signature color. "dorf" is the default in theme.css.
export const THEMES = [
  { id: "dorf", label: "Dorf", shiki: "github-dark-default" },
  { id: "github-dark-default", label: "GitHub Dark", accent: "textLink.foreground" },
  { id: "one-dark-pro", label: "One Dark Pro", accent: "textLink.foreground" },
  { id: "dracula", label: "Dracula", accent: "terminal.ansiMagenta" },
  { id: "tokyo-night", label: "Tokyo Night", accent: "terminal.ansiBlue" },
  { id: "nord", label: "Nord", accent: "textLink.foreground" },
  { id: "gruvbox-dark-medium", label: "Gruvbox", accent: "terminal.ansiYellow" },
  { id: "catppuccin-mocha", label: "Catppuccin Mocha", accent: "focusBorder" },
  { id: "rose-pine", label: "Rosé Pine", accent: "button.background" },
  { id: "kanagawa-wave", label: "Kanagawa", accent: "terminal.ansiYellow" },
  { id: "everforest-dark", label: "Everforest", accent: "textLink.foreground" },
  { id: "ayu-dark", label: "Ayu Dark", accent: "textLink.foreground" },
  { id: "night-owl", label: "Night Owl", accent: "terminal.ansiBlue" },
  { id: "monokai", label: "Monokai", accent: "terminal.ansiMagenta" },
  { id: "solarized-dark", label: "Solarized Dark", accent: "terminal.ansiBlue" },
  { id: "vitesse-black", label: "Vitesse Black", accent: "textLink.foreground" },
  { id: "github-light-default", label: "GitHub Light", accent: "textLink.foreground" },
  { id: "catppuccin-latte", label: "Catppuccin Latte", accent: "textLink.foreground" },
  { id: "solarized-light", label: "Solarized Light", accent: "terminal.ansiBlue" },
  { id: "rose-pine-dawn", label: "Rosé Pine Dawn", accent: "button.background" },
  { id: "one-light", label: "One Light", accent: "focusBorder" },
] as const

export type ThemeId = (typeof THEMES)[number]["id"]
const KEY = "dorf.theme"
const listeners = new Set<() => void>()
let preview: ThemeId | null = null

function read(): ThemeId {
  try {
    const id = localStorage.getItem(KEY)
    if (THEMES.some((t) => t.id === id)) return id as ThemeId
  } catch {}
  return "dorf"
}

// The theme on screen: a preview while the palette highlights one, else the saved choice.
export function useThemeId(): ThemeId {
  return useSyncExternalStore(
    (f) => (listeners.add(f), () => listeners.delete(f)),
    () => preview ?? read(),
    () => "dorf",
  )
}

export function savedTheme(): ThemeId {
  return read()
}

export function previewTheme(id: ThemeId | null): void {
  preview = id
  listeners.forEach((f) => f())
}

export function shikiTheme(id: ThemeId): string {
  const t = THEMES.find((x) => x.id === id)!
  return "shiki" in t ? t.shiki : t.id
}

const mix = (a: string, pct: number, b: string) => `color-mix(in oklab, ${a} ${pct}%, ${b})`

export function chooseTheme(id: ThemeId): void {
  try {
    localStorage.setItem(KEY, id)
  } catch {}
  preview = null
  listeners.forEach((f) => f())
}

// Writes the theme as inline variables on <html>, which win over theme.css; Dorf clears them.
export async function applyTheme(id: ThemeId, setMode: (mode: "dark" | "light") => void): Promise<void> {
  const root = document.documentElement
  const t = THEMES.find((x) => x.id === id)!
  for (const name of Array.from(root.style)) if (name.startsWith("--")) root.style.removeProperty(name)
  if (!("accent" in t)) return setMode("dark")
  const { bundledThemes } = await import("shiki")
  const theme = (await bundledThemes[t.id as keyof typeof bundledThemes]()).default
  const c = (theme.colors ?? {}) as Record<string, string>
  const bg = c["editor.background"]!
  const fg = c["editor.foreground"] ?? (theme.type === "dark" ? "#e6e6e6" : "#1f1f1f")
  const accent = c[t.accent] ?? c["textLink.foreground"] ?? fg
  const side = c["sideBar.background"] && c["sideBar.background"] !== bg ? c["sideBar.background"] : mix(bg, 88, theme.type === "dark" ? "black" : "white")
  const raised = (pct: number) => mix(fg, pct, bg)
  const vars: Record<string, string> = {
    "--fluid-accent": accent,
    "--background": bg,
    "--foreground": fg,
    "--card": raised(5),
    "--card-foreground": fg,
    "--popover": raised(7),
    "--popover-foreground": fg,
    "--primary": accent,
    "--primary-foreground": bg,
    "--secondary": raised(7),
    "--secondary-foreground": fg,
    "--muted": raised(7),
    "--muted-foreground": mix(fg, 60, bg),
    "--accent": raised(7),
    "--accent-foreground": fg,
    "--destructive": c["terminal.ansiRed"] ?? c["errorForeground"] ?? "#e5534b",
    "--border": raised(12),
    "--input": raised(16),
    "--ring": accent,
    "--sidebar": side,
    "--sidebar-foreground": fg,
    "--sidebar-primary": accent,
    "--sidebar-primary-foreground": bg,
    "--sidebar-accent": raised(9),
    "--sidebar-accent-foreground": fg,
    "--sidebar-border": raised(12),
    "--sidebar-ring": accent,
    "--surface-1": side,
    "--surface-2": bg,
    "--surface-3": raised(7),
    "--surface-4": raised(12),
    "--surface-5": raised(18),
    "--surface-6": raised(24),
    "--surface-7": raised(24),
    "--surface-8": raised(24),
  }
  for (const [k, v] of Object.entries(vars)) root.style.setProperty(k, v)
  setMode(theme.type === "light" ? "light" : "dark")
}
