"use client"

import { useState } from "react"
import { Badge } from "@/components/ui/badge"
import { Elapsed } from "@/components/dorf/elapsed"
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
import type { Thread } from "@/lib/dorf"

// As in T3 Code: a colored dot and a word before the title, only while the Thread works or has news. The open
// Thread shows no news, since the owner is looking at it.
const STATUS = {
  waiting: { color: "amber", label: "Waiting" },
  starting: { color: "blue", label: "Starting" },
  working: { color: "blue", label: "Working" },
  failed: { color: "red", label: "Failed" },
  done: { color: "emerald", label: "Completed" },
} as const
const Status = ({ thread, open }: { thread: Thread; open: boolean }) => {
  const status = thread.waiting ? "waiting" : thread.starting ? "starting" : thread.working ? "working" : open ? undefined : thread.attention
  if (!status) return null
  return (
    <Badge variant="ghost" size="compact" color={STATUS[status].color} title={status === "waiting" ? "Waiting for a machine: the others are working" : undefined}
      className={status === "working" || status === "starting" ? "shrink-0 [&>span:first-child]:animate-pulse" : "shrink-0"}>
      {STATUS[status].label}
      {thread.waiting && (
        <>
          {" "}
          <Elapsed since={thread.waiting.since} />
        </>
      )}
    </Badge>
  )
}

const Title = ({ thread }: { thread: Thread }) => (
  <span className="truncate">
    {thread.repo && <span className="text-muted-foreground">{thread.repo.split("/")[1]} · </span>}
    {thread.title}
  </span>
)

type ThreadSidebarProps = { threads: Thread[]; current?: number; connection: number; onUnarchive: (id: number) => void; onDelete: (id: number) => void }

// Active Threads, then the archived ones folded at the bottom. Only an archived Thread can be deleted, and the bin
// asks once more in place: a second click deletes.
export function ThreadSidebar({ threads, current, connection, onUnarchive, onDelete }: ThreadSidebarProps) {
  const icons = useIcons()
  const [confirming, setConfirming] = useState<number>()
  const active = threads.filter((t) => !t.archived)
  const archived = threads.filter((t) => t.archived)
  const { isMobile, setOpenMobile } = useSidebar()
  const go = (hash: string) => {
    window.location.hash = hash
    if (isMobile) setOpenMobile(false)
  }
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
        {active.length > 0 && (
          <SidebarGroup>
            <SidebarGroupLabel>Threads</SidebarGroupLabel>
            <SidebarMenu>
              {active.map((t) => (
                <SidebarMenuItem key={t.id}>
                  <SidebarMenuButton isActive={t.id === current} onClick={() => go(String(t.id))}>
                    <Status thread={t} open={t.id === current} />
                    <Title thread={t} />
                  </SidebarMenuButton>
                </SidebarMenuItem>
              ))}
            </SidebarMenu>
          </SidebarGroup>
        )}
        {archived.length > 0 && (
          <SidebarGroup collapsible defaultOpen={false}>
            <SidebarGroupLabel>Archived</SidebarGroupLabel>
            <SidebarMenu>
              {archived.map((t) => (
                <SidebarMenuItem key={t.id}>
                  <SidebarMenuButton isActive={t.id === current} onClick={() => go(String(t.id))}>
                    {confirming === t.id ? <span className="truncate text-red-600 dark:text-red-400">Delete for good?</span> : <Title thread={t} />}
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
