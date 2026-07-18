import { Outlet, createRootRoute } from "@tanstack/react-router";
import { SidebarInset, SidebarProvider } from "@/components/ui/sidebar";
import { AppSidebar } from "@/components/app-sidebar";
import { AuthGate } from "@/components/auth-gate";
import { SiteHeader } from "@/components/site-header";
import { Toaster } from "@/components/ui/sonner";

// The root route provides the shell (sidebar + header + toaster) once the
// bearer has been verified. Every child route renders inside the sidebar
// inset. Route-level React Query / suspense boundaries are opted into by
// individual pages that want them.
export const rootRoute = createRootRoute({
  component: () => (
    <AuthGate>
      <SidebarProvider
        style={
          {
            "--sidebar-width": "calc(var(--spacing) * 72)",
            "--header-height": "calc(var(--spacing) * 12)",
          } as React.CSSProperties
        }
      >
        <AppSidebar variant="inset" />
        <SidebarInset>
          <SiteHeader />
          <main className="@container/main flex flex-1 flex-col gap-4 p-4 md:gap-6 md:p-6">
            <Outlet />
          </main>
        </SidebarInset>
      </SidebarProvider>
      <Toaster richColors position="top-right" />
    </AuthGate>
  ),
});
