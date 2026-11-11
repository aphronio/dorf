"use client"

import { useEffect, useState } from "react"
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuAction,
  SidebarMenuActions,
  SidebarMenuButton,
  SidebarMenuItem,
  useSidebar,
} from "@/components/ui/sidebar"
import { ClaudeStatus } from "@/components/dorf/claude-status"
import { useIcons } from "@/lib/icon-context"
import { fontWeights } from "@/lib/font-weight"
import type { Thread } from "@/lib/dorf"

type Attention = NonNullable<Thread["attention"]>

const MINUTE = 60_000
const time = (iso: string | undefined) => (iso ? Date.parse(iso) : 0)
// "3m", "2h", "4d": the short form fits one column; the row's label spells it out.
const short = (ms: number) => (ms < MINUTE ? "<1m" : ms < 60 * MINUTE ? `${Math.floor(ms / MINUTE)}m` : ms < 24 * 60 * MINUTE ? `${Math.floor(ms / (60 * MINUTE))}h` : `${Math.floor(ms / (24 * 60 * MINUTE))}d`)
const long = (ms: number) => {
  const [n, unit] = ms < MINUTE ? [0, ""] : ms < 60 * MINUTE ? [Math.floor(ms / MINUTE), "minute"] : ms < 24 * 60 * MINUTE ? [Math.floor(ms / (60 * MINUTE)), "hour"] : [Math.floor(ms / (24 * 60 * MINUTE)), "day"]
  return n ? `${n} ${unit}${n > 1 ? "s" : ""}` : "under a minute"
}

// The minute the times count from, kept current while the page is open.
function useNow() {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), MINUTE / 2)
    return () => clearInterval(timer)
  }, [])
  return now
}

// What the Thread is doing, for how long or since when, and that in words. A new Thread first waits for a machine,
// then starts, then works.
function stateOf(thread: Thread, attention: Attention | undefined, now: number) {
  const state = thread.waiting ? "waiting" : thread.starting ? "starting" : thread.working ? "working" : attention
  const from = thread.waiting ? thread.waiting.since : thread.starting ? thread.createdAt : thread.working ? thread.startedAt : (thread.endedAt ?? thread.createdAt)
  const since = now - time(from)
  const said = {
    waiting: `Waiting ${long(since)} for a machine`,
    starting: `Starting for ${long(since)}`,
    working: `Working for ${long(since)}`,
    failed: `Failed ${long(since)} ago`,
    done: `Done ${long(since)} ago`,
    idle: `Last ran ${long(since)} ago`,
  }[state ?? "idle"]
  return { since, state, said }
}

// The right-hand column: the time, then the state's icon in a fixed slot, so times end on one line and icons stand
// in one column. Shape and color both carry the state.
function Row({ thread, attention, now }: { thread: Thread; attention?: Attention; now: number }) {
  const icons = useIcons()
  const repo = thread.repo?.split("/")[1]
  const { since, state, said } = stateOf(thread, attention, now)
  const Icon = state === "waiting" ? icons.clock : state === "working" || state === "starting" ? icons["circle-dashed"] : state === "failed" ? icons["circle-alert"] : state === "done" ? icons["circle-check"] : undefined
  return (
    <span className="grid min-w-0 flex-1 grid-cols-[minmax(0,1fr)_auto] grid-rows-2 items-center gap-x-3" aria-hidden>
      <span
        className={attention ? "truncate text-foreground" : "truncate"}
        style={attention ? { fontVariationSettings: fontWeights.semibold } : undefined}
      >
        {thread.title}
      </span>
      <span className="flex items-center justify-end gap-1.5 text-[length:var(--fs-caption,12px)] tabular-nums text-muted-foreground" title={said}>
        {short(since)}
        <span className="flex size-3.5 items-center justify-center" aria-hidden>
          {Icon && (
            <Icon
              size={14}
              strokeWidth={2}
              className={
                state === "working" || state === "starting"
                  ? "text-blue-500 motion-safe:animate-[spin_2s_linear_infinite]"
                  : state === "waiting"
                    ? "text-amber-500"
                    : state === "failed"
                      ? "text-red-500"
                      : "text-emerald-500"
              }
            />
          )}
        </span>
      </span>
      <span className="truncate text-[length:var(--fs-caption,12px)] text-muted-foreground">{repo ?? "no repository"}</span>
    </span>
  )
}

type ThreadSidebarProps = { threads: Thread[]; current?: number; connection: number; onUnarchive: (id: number) => void; onDelete: (id: number) => void }

// Two sections, as a priority inbox: Threads whose run ended since the owner last looked, failed first, then every
// other Thread by when the owner last asked in it. A row moves when it crosses between them or when the owner sends,
// never because an agent made progress. The open Thread stays where it was until the owner leaves it, so it doesn't
// jump while read. Archived Threads fold at the bottom; only they can be deleted, and the bin asks once more in place.
export function ThreadSidebar({ threads, current, connection, onUnarchive, onDelete }: ThreadSidebarProps) {
  const icons = useIcons()
  const now = useNow()
  const [confirming, setConfirming] = useState<number>()
  // What the Thread asked for when the owner opened it; set only on opening, so a run ending while the owner reads
  // the Thread doesn't move it up.
  const [opened, setOpened] = useState<number>()
  const [held, setHeld] = useState<{ id: number; attention: Attention }>()
  if (opened !== current) {
    setOpened(current)
    const asked = threads.find((t) => t.id === current)?.attention
    setHeld(current !== undefined && asked ? { id: current, attention: asked } : undefined)
  }
  const attentionOf = (t: Thread) => (t.id === held?.id ? held.attention : t.id === current ? undefined : t.attention)
  const active = threads.filter((t) => !t.archived)
  const needsYou = active
    .filter((t) => attentionOf(t))
    .sort((a, b) => Number(attentionOf(b) === "failed") - Number(attentionOf(a) === "failed") || time(b.endedAt) - time(a.endedAt))
  const rest = active.filter((t) => !attentionOf(t)).sort((a, b) => time(b.askedAt ?? b.createdAt) - time(a.askedAt ?? a.createdAt))
  const archived = threads.filter((t) => t.archived)
  const { isMobile, setOpenMobile } = useSidebar()
  const go = (hash: string) => {
    window.location.hash = hash
    if (isMobile) setOpenMobile(false)
  }
  // Hovering shows what the row cuts off; a screen reader hears the row as one sentence.
  const label = (t: Thread, attention: Attention | undefined) => ({
    title: `${t.title}\n${t.repo ?? "No repository"}`,
    "aria-label": `${t.title}, ${t.repo ?? "no repository"}, ${stateOf(t, attention, now).said}`,
  })
  const row = (t: Thread) => (
    <SidebarMenuItem key={t.id}>
      <SidebarMenuButton size="lg" isActive={t.id === current} onClick={() => go(String(t.id))} {...label(t, attentionOf(t))}>
        <Row thread={t} attention={attentionOf(t)} now={now} />
      </SidebarMenuButton>
    </SidebarMenuItem>
  )
  return (
    <Sidebar variant="inset">
      <SidebarHeader>
        <div className="px-2 pt-1 pb-2 text-sm font-semibold text-[color:var(--fluid-accent)]">Dorf</div>
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton icon={icons.plus} isActive={current === undefined} onClick={() => go("")}>
              New Thread
            </SidebarMenuButton>
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarHeader>
      <SidebarContent>
        {needsYou.length > 0 && (
          <SidebarGroup>
            <SidebarGroupLabel>
              Needs you
              <span className="ml-auto tabular-nums">{needsYou.length}</span>
            </SidebarGroupLabel>
            <SidebarMenu>{needsYou.map(row)}</SidebarMenu>
          </SidebarGroup>
        )}
        {rest.length > 0 && (
          <SidebarGroup>
            <SidebarGroupLabel>Threads</SidebarGroupLabel>
            <SidebarMenu>{rest.map(row)}</SidebarMenu>
          </SidebarGroup>
        )}
        {archived.length > 0 && (
          <SidebarGroup collapsible defaultOpen={false}>
            <SidebarGroupLabel>Archived</SidebarGroupLabel>
            <SidebarMenu>
              {archived.map((t) => (
                <SidebarMenuItem key={t.id}>
                  <SidebarMenuButton size="lg" isActive={t.id === current} onClick={() => go(String(t.id))} {...label(t, undefined)}>
                    {confirming === t.id ? <span className="truncate text-red-600 dark:text-red-400">Delete for good?</span> : <Row thread={t} now={now} />}
                  </SidebarMenuButton>
                  <SidebarMenuActions>
                    <SidebarMenuAction aria-label="Unarchive" title="Unarchive" onClick={() => onUnarchive(t.id)}>
                      <icons.unarchive />
                    </SidebarMenuAction>
                    <SidebarMenuAction
                      aria-label={confirming === t.id ? "Delete for good" : "Delete"}
                      title={confirming === t.id ? "Click again to delete" : "Delete"}
                      className={confirming === t.id ? "!text-red-600 dark:!text-red-400" : undefined}
                      onBlur={() => setConfirming(undefined)}
                      onClick={() => (confirming === t.id ? (setConfirming(undefined), onDelete(t.id)) : setConfirming(t.id))}
                    >
                      <icons.delete />
                    </SidebarMenuAction>
                  </SidebarMenuActions>
                </SidebarMenuItem>
              ))}
            </SidebarMenu>
          </SidebarGroup>
        )}
      </SidebarContent>
      <SidebarFooter>
        <ClaudeStatus key={connection} />
      </SidebarFooter>
    </Sidebar>
  )
}
