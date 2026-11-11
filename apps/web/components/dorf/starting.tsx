"use client"

import { ChatMessage } from "@/components/ui/chat-message"
import { Elapsed } from "@/components/dorf/elapsed"
import type { Thread } from "@/lib/dorf"

// The first message while the agent starts the Thread's machine and clones its repository, and what kept it from
// getting in, if anything did.
// While no machine is free it says for how long, and which Threads hold the machines.
export function Starting({ prompt, images, repo, waiting, busy = [], failure }: { prompt: string; images: File[]; repo?: string; waiting?: Thread["waiting"]; busy?: Thread[]; failure?: string }) {
  return (
    <div className="min-h-0 flex-1 overflow-y-auto">
      <div className="mx-auto flex w-full max-w-3xl flex-col gap-4 px-4 py-6">
        <ChatMessage from="user" className="whitespace-pre-wrap" files={images}>{prompt}</ChatMessage>
        {failure ? (
          <p className="font-mono text-[13px] text-red-600 dark:text-red-400">Could not start: {failure}</p>
        ) : (
          <p className="flex items-start gap-2 font-mono text-[13px] text-muted-foreground">
            <span className="mt-[7px] size-1.5 shrink-0 animate-pulse rounded-full bg-[var(--fluid-accent)]" />
            {waiting ? (
              <span>
                Waiting <Elapsed since={waiting.since} /> for a machine. It starts when one is free.
                {busy.length > 0 && (
                  <>
                    <br />
                    {busy.length === 1 ? "1 machine is" : `${busy.length} machines are`} busy with {busy.map((t) => `“${t.title}”`).join(", ")}.
                  </>
                )}
              </span>
            ) : repo
                ? `Starting a machine and cloning ${repo}…`
                : "Starting a machine…"}
          </p>
        )}
      </div>
    </div>
  )
}
