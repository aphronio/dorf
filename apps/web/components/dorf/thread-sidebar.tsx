"use client"

import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  useSidebar,
} from "@/components/ui/sidebar"
import { ClaudeStatus } from "@/components/dorf/claude-status"
import { ThemePicker } from "@/components/dorf/theme-picker"
import { useIcons } from "@/lib/icon-context"
import type { Thread } from "@/lib/dorf"

export function ThreadSidebar({ threads, current }: { threads: Thread[]; current?: number }) {
  const icons = useIcons()
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
        {threads.length > 0 && (
          <SidebarGroup>
            <SidebarGroupLabel>Threads</SidebarGroupLabel>
            <SidebarMenu>
              {threads.map((t) => (
                <SidebarMenuItem key={t.id}>
                  <SidebarMenuButton isActive={t.id === current} onClick={() => go(String(t.id))}>
                    <span className="truncate">
                      {t.repo && <span className="text-muted-foreground">{t.repo.split("/")[1]} · </span>}
                      {t.title}
                    </span>
                  </SidebarMenuButton>
                </SidebarMenuItem>
              ))}
            </SidebarMenu>
          </SidebarGroup>
        )}
      </SidebarContent>
      <SidebarFooter>
        <ThemePicker />
        <ClaudeStatus />
      </SidebarFooter>
    </Sidebar>
  )
}
