"use client"

import { Select, SelectContent, SelectItem, SelectTrigger } from "@/components/ui/select"

// A borderless menu for the composer's row: repository, model, thinking.
export function Picker({ value, options, onChange, label }: { value: string; options: { value: string; label: string }[]; onChange: (value: string) => void; label: string }) {
  return (
    <Select value={value} onValueChange={(v) => onChange(v as string)}>
      <SelectTrigger variant="borderless" className="w-auto max-w-56 min-w-0" aria-label={label} />
      <SelectContent>
        {options.map((o, i) => (
          <SelectItem key={o.value} value={o.value} index={i}>
            {o.label}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  )
}
