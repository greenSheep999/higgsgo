import { useMatches } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { Separator } from "@/components/ui/separator";
import { SidebarTrigger } from "@/components/ui/sidebar";
import { ThemeToggle } from "@/components/theme-toggle";
import { LanguageToggle } from "@/components/language-toggle";

// SiteHeader shows the last matched route's title (fed by each route via
// staticData.title) so the shell always agrees with the sidebar item that's
// highlighted. Falls back to "higgsgo".
export function SiteHeader() {
  const matches = useMatches();
  const { t } = useTranslation();
  // Each route declares its title via staticData.titleKey (an i18n path).
  // Legacy `staticData.title` (raw string) is honoured as a fallback so
  // the header keeps working before every route is migrated.
  const meta = matches
    .map((m) => m.staticData as { titleKey?: string; title?: string } | undefined)
    .filter((m): m is { titleKey?: string; title?: string } => !!m)
    .pop();
  const title = meta?.titleKey
    ? t(meta.titleKey)
    : (meta?.title ?? "higgsgo");

  return (
    <header className="flex h-(--header-height) shrink-0 items-center gap-2 border-b transition-[width,height] ease-linear group-has-data-[collapsible=icon]/sidebar-wrapper:h-(--header-height)">
      <div className="flex w-full items-center gap-1 px-4 lg:gap-2 lg:px-6">
        <SidebarTrigger className="-ml-1" />
        <Separator
          orientation="vertical"
          className="mx-2 data-[orientation=vertical]:h-4"
        />
        <h1 className="text-base font-medium">{title}</h1>
        <div className="ml-auto flex items-center gap-1">
          <LanguageToggle />
          <ThemeToggle />
        </div>
      </div>
    </header>
  );
}
