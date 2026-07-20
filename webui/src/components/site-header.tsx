import { useState } from "react";
import { useMatches } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { useMutation, useQuery } from "@tanstack/react-query";
import {
  IconCloudDownload,
  IconExternalLink,
  IconKey,
  IconScale,
  IconSettings,
} from "@tabler/icons-react";

import { Separator } from "@/components/ui/separator";
import { SidebarTrigger } from "@/components/ui/sidebar";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { ThemeToggle } from "@/components/theme-toggle";
import { LanguageToggle } from "@/components/language-toggle";
import { BearerDialog } from "@/components/settings/bearer-dialog";
import { LoadBalanceDialog } from "@/components/settings/load-balance-dialog";
import { admin, type VersionCheckResult, type VersionInfo } from "@/lib/api";

// SiteHeader shows the last matched route's title (fed by each route via
// staticData.title) so the shell always agrees with the sidebar item that's
// highlighted. Falls back to "Higgs.go".
//
// Right-side icon order (fixed): update / language / theme / settings.
// - Update button is a placeholder toast until /admin/version/check is
//   wired end-to-end (backend has landed; UI hookup below is the next
//   slice).
// - Settings gear opens a dropdown with "update login key" and a live
//   "using DB · ····abcd" summary pulled from /admin/settings/bearer.
export function SiteHeader() {
  const matches = useMatches();
  const { t } = useTranslation();
  const [bearerOpen, setBearerOpen] = useState(false);
  const [loadBalanceOpen, setLoadBalanceOpen] = useState(false);

  const meta = matches
    .map((m) => m.staticData as { titleKey?: string; title?: string } | undefined)
    .filter((m): m is { titleKey?: string; title?: string } => !!m)
    .pop();
  const title = meta?.titleKey
    ? t(meta.titleKey)
    : (meta?.title ?? "Higgs.go");

  const bearerMetaQ = useQuery({
    queryKey: ["admin", "settings", "bearer"],
    queryFn: admin.getBearerSettings,
    staleTime: 60_000,
  });

  const summary = bearerMetaQ.data
    ? t("settings.bearerDialog.currentSummary", {
        last4: bearerMetaQ.data.last_4 || "—",
        source:
          bearerMetaQ.data.source === "db"
            ? t("settings.sourceDb")
            : t("settings.sourceToml"),
      })
    : t("settings.bearerDialog.currentUnknown");

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
          <UpdateButton />
          <LanguageToggle />
          <ThemeToggle />
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button variant="ghost" size="icon" aria-label={t("settings.menu")}>
                <IconSettings className="size-4" />
                <span className="sr-only">{t("settings.menu")}</span>
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end" className="w-64">
              <DropdownMenuLabel>{t("settings.menu")}</DropdownMenuLabel>
              <DropdownMenuItem onSelect={() => setBearerOpen(true)}>
                <IconKey className="size-4" />
                <span>{t("settings.updateBearer")}</span>
              </DropdownMenuItem>
              <DropdownMenuItem onSelect={() => setLoadBalanceOpen(true)}>
                <IconScale className="size-4" />
                <span>{t("settings.loadBalanceAdvanced")}</span>
              </DropdownMenuItem>
              <DropdownMenuSeparator />
              <div className="px-2 py-1.5 text-[11px] text-muted-foreground">
                {summary}
              </div>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </div>
      <BearerDialog
        open={bearerOpen}
        onOpenChange={setBearerOpen}
        current={bearerMetaQ.data ?? null}
      />
      <LoadBalanceDialog
        open={loadBalanceOpen}
        onOpenChange={setLoadBalanceOpen}
      />
    </header>
  );
}

// UpdateButton — right-header icon that pops a dialog with the current
// build version and, when possible, checks GitHub Releases for a newer
// tag. Cheap on the server (1h cache), so we call on every click without
// worrying about rate limits. Dev builds short-circuit to "you're on
// dev, no check performed".
function UpdateButton() {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const versionQ = useQuery({
    queryKey: ["admin", "version"],
    queryFn: admin.getVersion,
    staleTime: 60 * 60_000,
  });
  const check = useMutation({
    mutationFn: admin.checkVersion,
  });
  const openDialog = () => {
    setOpen(true);
    check.reset();
    check.mutate();
  };
  return (
    <>
      <Button
        variant="ghost"
        size="icon"
        onClick={openDialog}
        title={t("header.checkUpdate")}
      >
        <IconCloudDownload className="size-4" />
        <span className="sr-only">{t("header.checkUpdate")}</span>
      </Button>
      <UpdateDialog
        open={open}
        onOpenChange={setOpen}
        info={versionQ.data ?? null}
        result={check.data ?? null}
        isChecking={check.isPending}
        onRecheck={() => check.mutate()}
      />
    </>
  );
}

// UpdateDialog renders three states: checking, up-to-date, and
// update-available. The GitHub error path (network / rate limit) is
// shown as a soft warning so operators still see the current version
// even when the outbound call fails.
function UpdateDialog({
  open,
  onOpenChange,
  info,
  result,
  isChecking,
  onRecheck,
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
  info: VersionInfo | null;
  result: VersionCheckResult | null;
  isChecking: boolean;
  onRecheck: () => void;
}) {
  const { t } = useTranslation();
  const currentVersion = info?.version ?? result?.current ?? "—";
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{t("header.updateDialogTitle")}</DialogTitle>
          <DialogDescription>
            {t("header.updateDialogDescription")}
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-3 text-sm">
          {/* Always show the running build so operators can verify
              which binary they're talking to. Commit + build_time hide
              behind a details block to keep the primary line short. */}
          <div className="rounded-md border p-3">
            <div className="text-xs text-muted-foreground">
              {t("header.currentVersion")}
            </div>
            <div className="font-mono text-base">{currentVersion}</div>
            {info?.commit && info.commit !== "none" ? (
              <div className="mt-1 text-[11px] text-muted-foreground">
                <span className="font-mono">{info.commit}</span>
                {info.build_time && info.build_time !== "unknown"
                  ? " · " + info.build_time
                  : ""}
                {info.go_version ? " · " + info.go_version : ""}
                {info.os_arch ? " · " + info.os_arch : ""}
              </div>
            ) : null}
          </div>

          {/* Result panel: three mutually-exclusive states. */}
          {isChecking ? (
            <p className="text-sm text-muted-foreground">
              {t("header.checkingUpdate")}
            </p>
          ) : result?.dev ? (
            <p className="text-xs text-muted-foreground">
              {t("header.devBuildNoCheck")}
            </p>
          ) : result?.error ? (
            <p className="text-sm text-amber-600 dark:text-amber-400">
              {t("header.updateFailed")}
            </p>
          ) : result?.update_available ? (
            <div className="space-y-2 rounded-md border border-emerald-500/40 bg-emerald-500/5 p-3">
              <div className="text-sm font-medium text-emerald-700 dark:text-emerald-400">
                {t("header.updateAvailable", { version: result.latest ?? "?" })}
              </div>
              {result.published_at ? (
                <div className="text-[11px] text-muted-foreground">
                  {result.published_at}
                </div>
              ) : null}
              {result.release_url ? (
                <a
                  href={result.release_url}
                  target="_blank"
                  rel="noreferrer"
                  className="inline-flex items-center gap-1 text-xs text-primary underline-offset-2 hover:underline"
                >
                  {t("header.viewRelease")}
                  <IconExternalLink className="size-3" />
                </a>
              ) : null}
              <p className="text-[11px] text-muted-foreground">
                {t("header.upgradeHint")}
              </p>
            </div>
          ) : result ? (
            <p className="text-sm text-emerald-700 dark:text-emerald-400">
              {t("header.upToDate")}
            </p>
          ) : null}
        </div>

        <DialogFooter>
          <Button
            variant="outline"
            onClick={onRecheck}
            disabled={isChecking}
          >
            {t("header.recheck")}
          </Button>
          <Button onClick={() => onOpenChange(false)}>
            {t("common.cancel")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
