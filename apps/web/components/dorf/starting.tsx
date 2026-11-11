"use client"

import { ChatMessage } from "@/components/ui/chat-message"

// The first message while the agent starts the Thread's machine and clones its repository, and what kept it from
// getting in, if anything did.
export function Starting({ prompt, images, repo, waiting, failure }: { prompt: string; images: File[]; repo?: string; waiting?: boolean; failure?: string }) {
  return (
    <div className="min-h-0 flex-1 overflow-y-auto">
      <div className="mx-auto flex w-full max-w-3xl flex-col gap-4 px-4 py-6">
        <ChatMessage from="user" className="whitespace-pre-wrap" files={images}>{prompt}</ChatMessage>
        {failure ? (
          <p className="font-mono text-[13px] text-red-600 dark:text-red-400">Could not start: {failure}</p>
        ) : (
          <p className="flex items-center gap-2 font-mono text-[13px] text-muted-foreground">
            <span className="size-1.5 animate-pulse rounded-full bg-[var(--fluid-accent)]" />
            {waiting
              ? "Waiting for a machine: the others are working. It starts when one is free…"
              : repo
                ? `Starting a machine and cloning ${repo}…`
                : "Starting a machine…"}
          </p>
        )}
      </div>
    </div>
  )
}
