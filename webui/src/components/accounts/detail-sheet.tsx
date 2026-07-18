import { useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { admin, ApiError, type Account } from "@/lib/api";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { Badge } from "@/components/ui/badge";
import { Progress } from "@/components/ui/progress";
import { Separator } from "@/components/ui/separator";
import { Skeleton } from "@/components/ui/skeleton";
import { formatCredits, formatDateTime } from "@/lib/format";

interface Props {
  id: string | null;
  onOpenChange: (open: boolean) => void;
}

// AccountDetailSheet is a right-side drawer that pulls one account through
// /admin/accounts/{id} and shows every non-secret field grouped into logical
// sections with separators for scanability.
export function AccountDetailSheet({ id, onOpenChange }: Props) {
  const { t } = useTranslation();
  const q = useQuery({
    queryKey: ["admin", "accounts", id],
    queryFn: () => admin.getAccount(id!),
    enabled: !!id,
  });

  return (
    <Sheet open={!!id} onOpenChange={onOpenChange}>
      <SheetContent className="w-full sm:max-w-xl overflow-y-auto">
        <SheetHeader>
          <SheetTitle>{t("accounts.detail.title")}</SheetTitle>
          <SheetDescription>
            GET /admin/accounts/{id ?? "…"}
          </SheetDescription>
        </SheetHeader>

        <div className="mt-4 space-y-4 px-4 pb-4">
          {q.isLoading ? (
            <div className="space-y-2">
              <Skeleton className="h-4 w-3/4" />
              <Skeleton className="h-4 w-1/2" />
              <Skeleton className="h-4 w-2/3" />
            </div>
          ) : q.isError ? (
            <p className="text-sm text-destructive">
              {q.error instanceof ApiError
                ? `${q.error.status} ${q.error.type}: ${q.error.message}`
                : q.error instanceof Error
                  ? q.error.message
                  : t("common.couldNotLoad", {
                      what: t("accounts.detail.title"),
                    })}
            </p>
          ) : q.data && id ? (
            <>
              <DetailBody account={q.data} />
              <Separator />
              <EligibleModelsPanel accountId={id} />
              <Separator />
              <RecentJobsPanel accountId={id} />
            </>
          ) : null}
        </div>
      </SheetContent>
    </Sheet>
  );
}

// ---------------------------------------------------------------------------
// DetailBody — structured into logical sections
// ---------------------------------------------------------------------------

function DetailBody({ account }: { account: Account }) {
  const { t } = useTranslation();

  const totalAvailable = account.subscription_balance + account.credits_balance;
  const balancePercent =
    account.total_plan_credits > 0
      ? Math.round(
          (account.subscription_balance / account.total_plan_credits) * 100,
        )
      : 0;

  return (
    <div className="space-y-5">
      {/* Section 1: Identity */}
      <section>
        <SectionHeading>{t("accounts.detail.sectionIdentity")}</SectionHeading>
        <Grid>
          <Field label={t("accounts.detail.email")} value={account.email} />
          <Field label={t("accounts.detail.accountId")} value={account.id} mono />
          <Field
            label={t("accounts.detail.status")}
            value={<Badge>{account.status}</Badge>}
          />
          <Field
            label={t("accounts.detail.source")}
            value={account.source || "—"}
          />
          <Field
            label={t("accounts.detail.note")}
            value={account.note || "—"}
          />
        </Grid>
      </section>

      <Separator />

      {/* Section 2: Plan & Entitlements */}
      <section>
        <SectionHeading>{t("accounts.detail.sectionPlan")}</SectionHeading>
        <Grid>
          <Field
            label={t("accounts.detail.planType")}
            value={account.plan_type || "—"}
          />
          <Field
            label={t("accounts.detail.cohort")}
            value={account.cohort || "—"}
          />
          <Field
            label={t("accounts.detail.hasUnlim")}
            value={<BoolBadge value={account.has_unlim} />}
          />
          <Field
            label={t("accounts.detail.hasFlexUnlim")}
            value={<BoolBadge value={account.has_flex_unlim} />}
          />
          <Field
            label={t("accounts.detail.proVeo3")}
            value={<BoolBadge value={account.is_pro_veo3} />}
          />
          <Field
            label={t("accounts.detail.planEnds")}
            value={formatDateTime(account.plan_ends_at)}
          />
        </Grid>
      </section>

      <Separator />

      {/* Section 3: Balance */}
      <section>
        <SectionHeading>{t("accounts.detail.sectionBalance")}</SectionHeading>
        <Grid>
          <Field
            label={t("accounts.detail.subCredits")}
            value={formatCredits(account.subscription_balance)}
          />
          <Field
            label={t("accounts.detail.totalPlanCredits")}
            value={formatCredits(account.total_plan_credits)}
          />
          <Field
            label={t("accounts.detail.creditsBalance")}
            value={formatCredits(account.credits_balance)}
          />
          <Field
            label={t("accounts.detail.totalAvailable")}
            value={formatCredits(totalAvailable)}
          />
        </Grid>
        {account.total_plan_credits > 0 && (
          <div className="mt-3 space-y-1">
            <div className="flex justify-between text-xs text-muted-foreground">
              <span>{t("accounts.detail.subCredits")}</span>
              <span>{balancePercent}%</span>
            </div>
            <Progress value={balancePercent} className="h-1.5" />
          </div>
        )}
      </section>

      <Separator />

      {/* Section 4: Runtime */}
      <section>
        <SectionHeading>{t("accounts.detail.sectionRuntime")}</SectionHeading>
        <Grid>
          <Field
            label={t("accounts.detail.priority")}
            value={String(account.priority)}
          />
          <Field
            label={t("accounts.detail.inFlightJobs")}
            value={`${account.in_flight_jobs} / ${account.max_concurrent || "∞"}`}
          />
          <Field
            label={t("accounts.detail.failStreak")}
            value={String(account.fail_streak)}
          />
          <Field
            label={t("accounts.detail.lastFailed")}
            value={formatDateTime(account.last_failed_at)}
          />
          <Field
            label={t("accounts.detail.lastUsed")}
            value={formatDateTime(account.last_used_at)}
          />
        </Grid>
      </section>

      <Separator />

      {/* Section 5: Network & Session */}
      <section>
        <SectionHeading>{t("accounts.detail.sectionNetwork")}</SectionHeading>
        <Grid>
          <Field
            label={t("accounts.detail.boundProxy")}
            value={account.bound_proxy_url || "—"}
            mono
          />
          <Field
            label={t("accounts.detail.workspace")}
            value={account.workspace_id || "—"}
            mono
          />
        </Grid>
      </section>

      <Separator />

      {/* Section 6: Timestamps */}
      <section>
        <SectionHeading>{t("accounts.detail.sectionTimestamps")}</SectionHeading>
        <Grid>
          <Field
            label={t("accounts.detail.imported")}
            value={formatDateTime(account.imported_at)}
          />
          <Field
            label={t("accounts.detail.registered")}
            value={formatDateTime(account.registered_at)}
          />
          <Field
            label={t("accounts.detail.lastBalance")}
            value={formatDateTime(account.last_balance_at)}
          />
        </Grid>
      </section>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Shared layout components
// ---------------------------------------------------------------------------

function SectionHeading({ children }: { children: React.ReactNode }) {
  return (
    <h4 className="mb-2 text-sm font-semibold">{children}</h4>
  );
}

function Grid({ children }: { children: React.ReactNode }) {
  return (
    <div className="grid grid-cols-2 gap-x-4 gap-y-2 text-sm">{children}</div>
  );
}

function Field({
  label,
  value,
  mono,
}: {
  label: string;
  value: React.ReactNode;
  mono?: boolean;
}) {
  return (
    <div className="space-y-0.5">
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className={mono ? "font-mono text-xs break-all" : "text-sm"}>
        {value}
      </div>
    </div>
  );
}

function BoolBadge({ value }: { value: boolean }) {
  return value ? (
    <Badge variant="secondary">Yes</Badge>
  ) : (
    <span className="text-sm text-muted-foreground">No</span>
  );
}

// ---------------------------------------------------------------------------
// Sub-panels
// ---------------------------------------------------------------------------

// EligibleModelsPanel calls /admin/accounts/{id}/eligible-models — an
// account-level derivative of the model registry that folds in the
// account's plan/unlim flags. Renders as a small counter row + a
// scrollable badge cloud so the operator can eyeball what this account
// is entitled to without paging.
function EligibleModelsPanel({ accountId }: { accountId: string }) {
  const q = useQuery({
    queryKey: ["admin", "accounts", accountId, "eligible-models"],
    queryFn: () => admin.accountEligibleModels(accountId),
    staleTime: 60_000,
  });
  return (
    <section className="space-y-2">
      <h4 className="text-sm font-semibold">Eligible models</h4>
      {q.isLoading ? (
        <Skeleton className="h-8 w-full" />
      ) : q.isError || !q.data ? (
        <p className="text-xs text-muted-foreground">—</p>
      ) : (
        <>
          <div className="text-xs text-muted-foreground">
            {q.data.eligible} / {q.data.total} ({q.data.by_output.image} img ·{" "}
            {q.data.by_output.video} vid · {q.data.by_output.audio} audio)
          </div>
          <div className="flex max-h-32 flex-wrap gap-1 overflow-y-auto rounded-md border p-2">
            {q.data.data.slice(0, 100).map((m) => (
              <span
                key={m.alias}
                className="inline-flex items-center gap-1 rounded bg-muted px-1.5 py-0.5 text-[10px]"
                title={`${m.jst} · est ${m.est_cost}`}
              >
                {m.alias}
              </span>
            ))}
          </div>
        </>
      )}
    </section>
  );
}

// RecentJobsPanel hits /admin/jobs?account_id=… with a tight limit so
// the operator can see if this account has been busy / erroring.
function RecentJobsPanel({ accountId }: { accountId: string }) {
  const q = useQuery({
    queryKey: ["admin", "accounts", accountId, "jobs"],
    queryFn: () => admin.listJobs({ account_id: accountId, limit: 10 }),
    staleTime: 20_000,
  });
  const rows = q.data?.data ?? [];
  return (
    <section className="space-y-2">
      <h4 className="text-sm font-semibold">Recent jobs (10)</h4>
      {q.isLoading ? (
        <Skeleton className="h-16 w-full" />
      ) : rows.length === 0 ? (
        <p className="text-xs text-muted-foreground">—</p>
      ) : (
        <ul className="space-y-1 text-xs">
          {rows.map((j) => (
            <li
              key={j.id}
              className="flex items-baseline justify-between gap-2 rounded border px-2 py-1"
            >
              <span className="min-w-0 truncate">
                <span className="font-mono text-[10px] text-muted-foreground">
                  {j.id.slice(0, 8)}
                </span>{" "}
                {j.model_alias}
              </span>
              <span className="tabular-nums text-muted-foreground">
                {j.status}
              </span>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
