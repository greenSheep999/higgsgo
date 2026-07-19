import { useState } from "react";
import { createRoute } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import {
  IconUserPlus,
  IconRefresh,
  IconReload,
} from "@tabler/icons-react";

import {
  admin,
  ApiError,
  type BulkEnqueueRegistrationRequest,
  type EnqueueRegistrationRequest,
  type Registration,
} from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import { rootRoute } from "@/routes/root";
import {
  Card,
  CardAction,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Skeleton } from "@/components/ui/skeleton";
import { StatusBadge, type StatusTone } from "@/components/ui/status-badge";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";

// Registrations page.
//
// Two modes discriminated by the ApiError.type = "registrar_disabled"
// envelope the Go admin handler emits:
//
//   - Slim build (default `go build`): the /admin/registrations query
//     comes back 503 with that envelope. We render an amber "opt in
//     with -tags register" panel; Enqueue is hidden entirely so
//     operators aren't tempted to click a doomed button.
//
//   - Full build (`-tags register`): the query returns the row list.
//     Operators can enqueue via the dialog and retry failed rows
//     from an action button on each row.
//
// Every mutation invalidates the list query so the table reflects
// the queue advancing (pending → running → success/failed).

function statusTone(status: string): StatusTone {
  switch (status) {
    case "success":
      return "success";
    case "running":
      return "info";
    case "failed":
      return "danger";
    case "pending":
      return "warning";
    default:
      return "muted";
  }
}

// isRetryable returns true when the row is in a state Retry can act
// on. Success is terminal (no point re-running); pending/running are
// already in flight (Retry would race the worker). Only failed rows
// can be productively re-queued.
function isRetryable(r: Registration): boolean {
  return r.status === "failed";
}

function Registrations() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [enqueueOpen, setEnqueueOpen] = useState(false);

  const q = useQuery<{ data: Registration[]; limit: number; offset: number }>({
    queryKey: ["admin", "registrations"],
    queryFn: () => admin.listRegistrations({ limit: 200 }),
    // Slim-build 503 is a stable operational state, not a transient
    // failure. Suppress retries so the amber panel appears instantly
    // instead of after three retry backoffs. Everything else keeps
    // the default retry (3 attempts).
    retry: (failureCount, error) => {
      if (error instanceof ApiError && error.type === "registrar_disabled") {
        return false;
      }
      return failureCount < 3;
    },
    // Auto-refresh so the operator sees pending → running → terminal
    // without pressing the refresh button. 5s matches the driver's
    // default poll cadence.
    refetchInterval: 5000,
  });

  const retry = useMutation({
    mutationFn: (id: string) => admin.retryRegistration(id),
    onSuccess: (_res, id) => {
      toast.success(t("registrations.toasts.retried", { id }));
      qc.invalidateQueries({ queryKey: ["admin", "registrations"] });
    },
    onError: (err) => toast.error(errMsg(err)),
  });

  const disabled =
    q.error instanceof ApiError && q.error.type === "registrar_disabled";

  const rows = q.data?.data ?? [];

  return (
    <>
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <IconUserPlus className="size-5" /> {t("registrations.title")}
          </CardTitle>
          <CardDescription>{t("registrations.description")}</CardDescription>
          <CardAction className="flex flex-wrap gap-2">
            <Button
              variant="outline"
              size="sm"
              onClick={() => q.refetch()}
              disabled={q.isFetching}
            >
              <IconRefresh /> {t("common.refresh")}
            </Button>
            {/* Enqueue only shows on full builds — on slim builds the
                button would 503 on submit, which is worse UX than
                hiding it and showing the opt-in panel below. */}
            {!disabled && (
              <Button
                variant="default"
                size="sm"
                onClick={() => setEnqueueOpen(true)}
              >
                <IconUserPlus /> {t("registrations.enqueue")}
              </Button>
            )}
          </CardAction>
        </CardHeader>
        <CardContent className="space-y-4">
          {disabled ? (
            <div className="rounded-md border border-amber-500/30 bg-amber-500/5 p-4 text-sm">
              <div className="mb-1 font-medium text-amber-700 dark:text-amber-300">
                {t("registrations.disabledTitle")}
              </div>
              <div className="text-muted-foreground">
                {t("registrations.disabledHint")}
              </div>
            </div>
          ) : (
            <div className="overflow-hidden rounded-md border">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>{t("registrations.columns.id")}</TableHead>
                    <TableHead>{t("registrations.columns.email")}</TableHead>
                    <TableHead>{t("registrations.columns.status")}</TableHead>
                    <TableHead>{t("registrations.columns.attempts")}</TableHead>
                    <TableHead>{t("registrations.columns.account")}</TableHead>
                    <TableHead>{t("registrations.columns.created")}</TableHead>
                    <TableHead>{t("registrations.columns.finished")}</TableHead>
                    <TableHead className="text-right">
                      {t("registrations.columns.actions")}
                    </TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {q.isLoading ? (
                    Array.from({ length: 3 }).map((_, i) => (
                      <TableRow key={i}>
                        <TableCell colSpan={8}>
                          <Skeleton className="h-6 w-full" />
                        </TableCell>
                      </TableRow>
                    ))
                  ) : rows.length === 0 ? (
                    <TableRow>
                      <TableCell
                        colSpan={8}
                        className="text-center text-sm text-muted-foreground"
                      >
                        {t("registrations.empty")}
                      </TableCell>
                    </TableRow>
                  ) : (
                    rows.map((r) => (
                      <TableRow key={r.id}>
                        <TableCell className="font-mono text-xs">{r.id}</TableCell>
                        <TableCell>{r.email}</TableCell>
                        <TableCell>
                          <StatusBadge tone={statusTone(r.status)}>
                            {t(`registrations.status.${r.status}`, r.status)}
                          </StatusBadge>
                          {r.last_error ? (
                            <div
                              className="mt-1 line-clamp-2 text-xs text-muted-foreground"
                              title={r.last_error}
                            >
                              {r.last_error}
                            </div>
                          ) : null}
                        </TableCell>
                        <TableCell className="tabular-nums">{r.attempts}</TableCell>
                        <TableCell className="font-mono text-xs">
                          {r.account_id || "—"}
                        </TableCell>
                        <TableCell className="text-xs text-muted-foreground">
                          {r.created_at ? formatDateTime(r.created_at) : "—"}
                        </TableCell>
                        <TableCell className="text-xs text-muted-foreground">
                          {r.finished_at ? formatDateTime(r.finished_at) : "—"}
                        </TableCell>
                        <TableCell className="text-right">
                          {isRetryable(r) ? (
                            <Button
                              variant="outline"
                              size="sm"
                              onClick={() => retry.mutate(r.id)}
                              disabled={retry.isPending}
                              title={t("registrations.retryHint")}
                            >
                              <IconReload className="size-3.5" />
                              <span className="hidden sm:inline">
                                {t("registrations.retry")}
                              </span>
                            </Button>
                          ) : null}
                        </TableCell>
                      </TableRow>
                    ))
                  )}
                </TableBody>
              </Table>
            </div>
          )}
        </CardContent>
      </Card>

      {/* Enqueue dialog — opens when user clicks the primary button.
          Mounted at the route level so its state survives table
          re-renders. Closed via Cancel or successful submit. */}
      <EnqueueDialog
        open={enqueueOpen}
        onOpenChange={setEnqueueOpen}
      />
    </>
  );
}

// EnqueueDialog is a two-tab dialog:
//   - "Single" — one row form for the classic case (test row, or
//     a manual OAuth registration that doesn't fit the bulk format).
//     Requires email + password + mailbox_client_id + mailbox_refresh_token
//     because the Node driver's password flow needs all four to
//     actually complete OTP verification.
//   - "Bulk" — a textarea for the higgsfield-register mailbox list
//     format (`email----password----client_id----refresh_token`, one
//     line per row). Server parses line-by-line and returns
//     per-line errors so a bad line doesn't abort the whole batch.
// See ROADMAP §5.4 P4-3d.
function EnqueueDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
}) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [tab, setTab] = useState<"single" | "bulk">("bulk");

  // --- Single-row form state ------------------------------------
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [oauthSource, setOauthSource] = useState<string>("");
  const [proxyURL, setProxyURL] = useState("");
  const [mailboxClientID, setMailboxClientID] = useState("");
  const [mailboxRefreshToken, setMailboxRefreshToken] = useState("");

  // --- Bulk form state ------------------------------------------
  const [bulkLines, setBulkLines] = useState("");
  const [bulkProxyURL, setBulkProxyURL] = useState("");

  const enqueue = useMutation({
    mutationFn: (body: EnqueueRegistrationRequest) =>
      admin.enqueueRegistration(body),
    onSuccess: (res) => {
      toast.success(t("registrations.toasts.enqueued", { id: res.id }));
      qc.invalidateQueries({ queryKey: ["admin", "registrations"] });
      onOpenChange(false);
      setEmail("");
      setPassword("");
      setOauthSource("");
      setProxyURL("");
      setMailboxClientID("");
      setMailboxRefreshToken("");
    },
    onError: (err) => toast.error(errMsg(err)),
  });

  const bulkEnqueue = useMutation({
    mutationFn: (body: BulkEnqueueRegistrationRequest) =>
      admin.bulkEnqueueRegistrations(body),
    onSuccess: (res) => {
      // Show a summary toast with skipped count so operators know a
      // partial-success happened even when the batch is huge.
      if (res.skipped.length === 0) {
        toast.success(
          t("registrations.toasts.bulkOk", { count: res.enqueued }),
        );
      } else {
        toast.warning(
          t("registrations.toasts.bulkPartial", {
            enqueued: res.enqueued,
            skipped: res.skipped.length,
            firstReason: res.skipped[0]?.reason ?? "",
            firstLine: res.skipped[0]?.line ?? 0,
          }),
        );
      }
      qc.invalidateQueries({ queryKey: ["admin", "registrations"] });
      onOpenChange(false);
      setBulkLines("");
      setBulkProxyURL("");
    },
    onError: (err) => toast.error(errMsg(err)),
  });

  const submitSingle = () => {
    const body: EnqueueRegistrationRequest = { email: email.trim() };
    if (password) body.password = password;
    if (oauthSource) body.oauth_source = oauthSource;
    if (proxyURL.trim()) body.proxy_url = proxyURL.trim();
    if (mailboxClientID.trim()) body.mailbox_client_id = mailboxClientID.trim();
    if (mailboxRefreshToken.trim())
      body.mailbox_refresh_token = mailboxRefreshToken.trim();
    enqueue.mutate(body);
  };

  const submitBulk = () => {
    const body: BulkEnqueueRegistrationRequest = { lines: bulkLines };
    if (bulkProxyURL.trim()) body.proxy_url = bulkProxyURL.trim();
    bulkEnqueue.mutate(body);
  };

  // Single-row validation: password flow needs everything, OAuth
  // needs only email. Mirror the server-side gate in the handler.
  const singleValid = (() => {
    if (email.trim() === "") return false;
    if (oauthSource !== "") return true; // OAuth path
    return (
      password.trim() !== "" &&
      mailboxClientID.trim() !== "" &&
      mailboxRefreshToken.trim() !== ""
    );
  })();

  const canSubmitBulk = bulkLines.trim() !== "" && !bulkEnqueue.isPending;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      {/* Wider than a normal dialog because the bulk textarea benefits
          from real horizontal room — mailbox refresh tokens are ~600
          chars per line. */}
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>{t("registrations.enqueue")}</DialogTitle>
          <DialogDescription>
            {t("registrations.enqueueDescription")}
          </DialogDescription>
        </DialogHeader>

        <Tabs
          value={tab}
          onValueChange={(v) => setTab(v as "single" | "bulk")}
          className="w-full"
        >
          <TabsList className="grid w-full grid-cols-2">
            <TabsTrigger value="bulk">
              {t("registrations.tabs.bulk")}
            </TabsTrigger>
            <TabsTrigger value="single">
              {t("registrations.tabs.single")}
            </TabsTrigger>
          </TabsList>

          {/* -------- Bulk import -------------------------------- */}
          <TabsContent value="bulk" className="space-y-3 py-2">
            <div>
              <Label
                htmlFor="reg-bulk-lines"
                className="text-xs text-muted-foreground"
              >
                {t("registrations.form.bulkLines")}
              </Label>
              <Textarea
                id="reg-bulk-lines"
                rows={10}
                spellCheck={false}
                className="mt-1 font-mono text-xs"
                placeholder="email@outlook.com----password----client_id----refresh_token"
                value={bulkLines}
                onChange={(e) => setBulkLines(e.target.value)}
              />
              <p className="mt-1 text-[10px] text-muted-foreground">
                {t("registrations.form.bulkHint")}
              </p>
            </div>
            <div className="grid grid-cols-[6rem_1fr] items-center gap-3">
              <Label
                htmlFor="reg-bulk-proxy"
                className="text-xs text-muted-foreground"
              >
                {t("registrations.form.proxy")}
              </Label>
              <Input
                id="reg-bulk-proxy"
                placeholder="socks5://user:pass@host:port"
                value={bulkProxyURL}
                onChange={(e) => setBulkProxyURL(e.target.value)}
              />
            </div>
          </TabsContent>

          {/* -------- Single row --------------------------------- */}
          <TabsContent value="single" className="space-y-3 py-2">
            <div className="grid grid-cols-[6rem_1fr] items-center gap-3">
              <Label
                htmlFor="reg-email"
                className="text-xs text-muted-foreground"
              >
                {t("registrations.form.email")}
              </Label>
              <Input
                id="reg-email"
                type="email"
                placeholder="user@example.com"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
              />
            </div>
            <div className="grid grid-cols-[6rem_1fr] items-center gap-3">
              <Label
                htmlFor="reg-password"
                className="text-xs text-muted-foreground"
              >
                {t("registrations.form.password")}
              </Label>
              <Input
                id="reg-password"
                type="text"
                placeholder="required for the password flow"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
              />
            </div>
            <div className="grid grid-cols-[6rem_1fr] items-center gap-3">
              <Label className="text-xs text-muted-foreground">
                {t("registrations.form.oauth")}
              </Label>
              <Select
                value={oauthSource || "password"}
                onValueChange={(v) =>
                  setOauthSource(v === "password" ? "" : v)
                }
              >
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="password">
                    {t("registrations.form.oauthPassword")}
                  </SelectItem>
                  <SelectItem value="google">Google</SelectItem>
                  <SelectItem value="github">GitHub</SelectItem>
                </SelectContent>
              </Select>
            </div>
            {/* Mailbox credentials — only visible/required on the
                password flow. Hidden on OAuth. */}
            {oauthSource === "" ? (
              <>
                <div className="grid grid-cols-[6rem_1fr] items-center gap-3">
                  <Label
                    htmlFor="reg-mbclient"
                    className="text-xs text-muted-foreground"
                  >
                    {t("registrations.form.mailboxClient")}
                  </Label>
                  <Input
                    id="reg-mbclient"
                    placeholder="Microsoft Graph app client_id"
                    value={mailboxClientID}
                    onChange={(e) => setMailboxClientID(e.target.value)}
                  />
                </div>
                <div className="grid grid-cols-[6rem_1fr] items-center gap-3">
                  <Label
                    htmlFor="reg-mbrefresh"
                    className="text-xs text-muted-foreground"
                  >
                    {t("registrations.form.mailboxRefresh")}
                  </Label>
                  <Input
                    id="reg-mbrefresh"
                    placeholder="Microsoft Graph refresh_token"
                    className="font-mono text-xs"
                    value={mailboxRefreshToken}
                    onChange={(e) => setMailboxRefreshToken(e.target.value)}
                  />
                </div>
              </>
            ) : null}
            <div className="grid grid-cols-[6rem_1fr] items-center gap-3">
              <Label
                htmlFor="reg-proxy"
                className="text-xs text-muted-foreground"
              >
                {t("registrations.form.proxy")}
              </Label>
              <Input
                id="reg-proxy"
                placeholder="socks5://user:pass@host:port"
                value={proxyURL}
                onChange={(e) => setProxyURL(e.target.value)}
              />
            </div>
            <p className="text-[10px] text-muted-foreground">
              {t("registrations.form.hint")}
            </p>
          </TabsContent>
        </Tabs>

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            {t("common.cancel")}
          </Button>
          {tab === "bulk" ? (
            <Button onClick={submitBulk} disabled={!canSubmitBulk}>
              {bulkEnqueue.isPending
                ? t("common.loading")
                : t("registrations.tabs.bulkSubmit")}
            </Button>
          ) : (
            <Button
              onClick={submitSingle}
              disabled={!singleValid || enqueue.isPending}
            >
              {enqueue.isPending
                ? t("common.loading")
                : t("registrations.enqueue")}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// errMsg mirrors the pattern used across other route files: unwrap
// ApiError for the structured shape, fall back to Error.message,
// then to a coerce-to-string as a last resort.
function errMsg(err: unknown): string {
  if (err instanceof ApiError) {
    return `${err.status} ${err.type}: ${err.message}`;
  }
  if (err instanceof Error) return err.message;
  return String(err);
}

export const registrationsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/registrations",
  staticData: { titleKey: "nav.registrations" },
  component: Registrations,
});
