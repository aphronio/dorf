"use client"

import { useEffect, useState } from "react"
import { shikiTheme, useThemeId } from "@/lib/themes"

// Highlights with Shiki once the language's grammar loads; plain text until then or if unknown.
export function Code({ code, lang }: { code: string; lang: string }) {
  const [html, setHtml] = useState<string>()
  const theme = shikiTheme(useThemeId())
  useEffect(() => {
    let live = true
    import("shiki")
      .then(({ codeToHtml }) =>
        codeToHtml(code, { lang, themes: { light: theme === "github-dark-default" ? "github-light-default" : theme, dark: theme }, defaultColor: false }),
      )
      .then((out) => live && setHtml(out))
      .catch(() => live && setHtml(undefined))
    return () => void (live = false)
  }, [code, lang, theme])
  const box = "my-2 overflow-x-auto rounded-md bg-surface-1 p-3 font-mono text-xs leading-relaxed [&_pre]:!bg-transparent"
  return html ? <div className={box} dangerouslySetInnerHTML={{ __html: html }} /> : <pre className={box}>{code}</pre>
}
