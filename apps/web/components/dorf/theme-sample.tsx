"use client"

import { ChatMessage } from "@/components/ui/chat-message"
import { Markdown } from "@/components/dorf/markdown"
import { ToolCalls } from "@/components/dorf/tool-call"

const REPLY = "The test failed on an empty list. Fixed `total`:\n\n```ts\nexport function total(items: number[]): number {\n  return items.reduce((sum, n) => sum + n, 0) // empty is 0\n}\n```"

// A small Thread in the previewed theme, since the palette hides the real one.
export function ThemeSample() {
  return (
    <div className="flex flex-col gap-2 border-t border-border bg-background px-4 py-3 text-left">
      <ChatMessage from="user">Fix the failing test</ChatMessage>
      <div className="-mx-2">
        <ToolCalls
          calls={[
            { id: "a", command: "npm test", shell: true, error: true },
            { id: "b", command: "sed -n 1,20p src/total.ts", shell: true },
            { id: "c", command: "npm test", shell: true },
          ]}
        />
      </div>
      <ChatMessage from="assistant" className="max-w-full">
        <Markdown>{REPLY}</Markdown>
      </ChatMessage>
    </div>
  )
}
