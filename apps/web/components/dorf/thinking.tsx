"use client"

import { useState } from "react"
import { Popover } from "@base-ui/react/popover"
import { Slider } from "@base-ui/react/slider"
import { Button } from "@/components/ui/button"
import { Elevated } from "@/lib/elevated"
import { useIcons } from "@/lib/icon-context"

// A slider over the levels the model offers, in a popover so the composer row stays short on a phone.
export function Thinking({ value, levels, onChange }: { value: string; levels: string[]; onChange: (level: string) => void }) {
  const [open, setOpen] = useState(false)
  const Chevron = useIcons()["chevron-down"]
  const at = levels.indexOf(value)
  const pick = (level: string | undefined) => level && level !== value && onChange(level)
  const x = (i: number) => `${(i / Math.max(levels.length - 1, 1)) * 100}%`
  return (
    <Popover.Root open={open} onOpenChange={setOpen}>
      <Popover.Trigger render={<Button variant="ghost" size="compact" trailingIcon={Chevron} active={open} aria-label="Thinking" />}>
        {value}
      </Popover.Trigger>
      <Popover.Portal>
        <Popover.Positioner side="top" align="start" sideOffset={6} className="z-[var(--z-popover,60)] outline-none">
          <Popover.Popup render={<Elevated offset={2} shadowLevel={3} className="w-60 rounded-xl px-6 pt-4 pb-2 outline-none" />}>
            {/* Saves once on release; the next agent event moves it to the saved level. */}
            <Slider.Root key={value} defaultValue={Math.max(at, 0)} min={0} max={levels.length - 1} step={1} onValueCommitted={(v) => pick(levels[v as number])}>
              <Slider.Control className="flex h-4 cursor-pointer items-center">
                <Slider.Track className="relative h-1 w-full rounded-full bg-muted-foreground/25">
                  <Slider.Indicator className="rounded-full bg-[var(--fluid-accent)]" />
                  {levels.map((l, i) => (
                    <span key={l} className="absolute top-1/2 size-1 -translate-1/2 rounded-full bg-muted-foreground/60" style={{ left: x(i) }} />
                  ))}
                  <Slider.Thumb
                    aria-label="Thinking"
                    getAriaValueText={(_, v) => levels[v] ?? ""}
                    className="size-3.5 rounded-full bg-[var(--fluid-accent)] shadow ring-2 ring-background outline-none focus-visible:ring-[color:var(--focus-ring,#6B97FF)]"
                  />
                </Slider.Track>
              </Slider.Control>
            </Slider.Root>
            <div className="relative mt-1 h-7">
              {levels.map((l, i) => (
                <button
                  key={l}
                  type="button"
                  onClick={() => pick(l)}
                  className={`absolute -translate-x-1/2 px-1 py-1 text-xs ${l === value ? "font-medium text-foreground" : "text-muted-foreground hover:text-foreground"}`}
                  style={{ left: x(i) }}
                >
                  {l}
                </button>
              ))}
            </div>
          </Popover.Popup>
        </Popover.Positioner>
      </Popover.Portal>
    </Popover.Root>
  )
}
