import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { createRoute } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { IconEdit, IconPlus, IconReload, IconTrash } from "@tabler/icons-react";

import {
  admin,
  type PurchaseBatchInput,
  type PurchaseBatchView,
} from "@/lib/api";
import { rootRoute } from "@/routes/root";
import {
  Card,
  CardAction,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
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
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { formatUSD } from "@/lib/format";

// PricingPurchaseBatchesPage renders the CRUD table for
// purchase_batches. The summary card at the top shows what the
// effective unit cost is RIGHT NOW (weighted average from these
// rows) and how it compares to the config fallback — that's the
// number `retail_below_floor` warnings are computed against.
function PricingPurchaseBatchesPage() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [editing, setEditing] = useState<PurchaseBatchView | "new" | null>(
    null,
  );
  const [search, setSearch] = useState("");

  const query = useQuery({
    queryKey: ["admin", "purchase-batches"],
    queryFn: () => admin.listPurchaseBatches(),
    staleTime: 30_000,
  });
  const rows = query.data?.data ?? [];
  const summary = query.data?.summary;

  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase();
    if (!q) return rows;
    return rows.filter(
      (r) =>
        r.source_channel.toLowerCase().includes(q) ||
        r.source_seller.toLowerCase().includes(q) ||
        r.plan_type.toLowerCase().includes(q) ||
        r.linked_account_email.toLowerCase().includes(q),
    );
  }, [rows, search]);

  const deleteMut = useMutation({
    mutationFn: (id: string) => admin.deletePurchaseBatch(id),
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: ["admin", "purchase-batches"] }),
  });

  return (
    <div className="mx-auto max-w-[1400px] space-y-4 p-4">
      <Card>
        <CardHeader>
          <CardTitle>
            {t("pricing.batches.title", {
              defaultValue: "Purchase batches",
            })}
          </CardTitle>
          <CardDescription>
            {t("pricing.batches.description", {
              defaultValue:
                "Every real-world purchase feeds the credit-weighted effective unit cost that drives the retail-below-floor warning.",
            })}
          </CardDescription>
          <CardAction>
            <Button
              variant="ghost"
              size="sm"
              onClick={() => query.refetch()}
              disabled={query.isFetching}
            >
              <IconReload
                className={query.isFetching ? "animate-spin" : undefined}
              />
              {t("common.refresh", { defaultValue: "Refresh" })}
            </Button>
            <Button size="sm" onClick={() => setEditing("new")}>
              <IconPlus />
              {t("pricing.batches.new", { defaultValue: "New batch" })}
            </Button>
          </CardAction>
        </CardHeader>
        <CardContent className="space-y-3">
          {summary ? <SummaryStrip summary={summary} /> : null}
          <Input
            placeholder={t("pricing.batches.filterPlaceholder", {
              defaultValue:
                "Filter channel / seller / plan / linked account",
            })}
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            className="max-w-sm"
          />
          {query.isLoading ? (
            <Skeleton className="h-96 w-full" />
          ) : (
            <BatchesTable
              rows={filtered}
              onEdit={(row) => setEditing(row)}
              onDelete={(id) => {
                if (
                  window.confirm(
                    t("pricing.batches.confirmDelete", {
                      defaultValue:
                        "Delete this batch? Prefer setting active=false to preserve audit trail.",
                    }),
                  )
                ) {
                  deleteMut.mutate(id);
                }
              }}
            />
          )}
        </CardContent>
      </Card>

      {editing !== null ? (
        <BatchDialog
          initial={editing === "new" ? null : editing}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            qc.invalidateQueries({ queryKey: ["admin", "purchase-batches"] });
          }}
        />
      ) : null}
    </div>
  );
}

// SummaryStrip renders the "effective unit cost is $X µ/cr across N
// batches; config fallback is $Y" band at the top of the page. This
// is the number the operator cares about most.
function SummaryStrip({
  summary,
}: {
  summary: NonNullable<
    Awaited<ReturnType<typeof admin.listPurchaseBatches>>["summary"]
  >;
}) {
  const { t } = useTranslation();
  const effective = summary.effective_unit_cost_micros;
  const fallback = summary.config_fallback_micros;
  const diverges = Math.abs(effective - fallback) > 500;
  return (
    <div className="flex flex-wrap items-center gap-3 rounded-md border bg-muted/40 p-3 text-sm">
      <div>
        <div className="text-xs text-muted-foreground">
          {t("pricing.batches.summary.effective", {
            defaultValue: "Effective unit cost",
          })}
        </div>
        <div className="font-mono text-base font-medium">
          {effective.toLocaleString()} µ/cr
        </div>
        <div className="text-[10px] text-muted-foreground">
          = {formatUSD(effective / 1_000_000)}/cr
        </div>
      </div>
      <div>
        <div className="text-xs text-muted-foreground">
          {t("pricing.batches.summary.source", {
            defaultValue: "Source",
          })}
        </div>
        {summary.fallback_applied ? (
          <Badge variant="outline">
            {t("pricing.batches.summary.configFallback", {
              defaultValue: "config fallback",
            })}
          </Badge>
        ) : (
          <Badge variant="secondary">
            {t("pricing.batches.summary.batches", {
              defaultValue: "{{n}} batches",
              n: summary.eligible_batches,
            })}
          </Badge>
        )}
      </div>
      <div>
        <div className="text-xs text-muted-foreground">
          {t("pricing.batches.summary.numerator", {
            defaultValue: "Total paid",
          })}
        </div>
        <div className="font-mono">
          {formatUSD(summary.total_paid_micros / 1_000_000)}
        </div>
      </div>
      <div>
        <div className="text-xs text-muted-foreground">
          {t("pricing.batches.summary.denominator", {
            defaultValue: "Total credits",
          })}
        </div>
        <div className="font-mono">{summary.total_credits.toLocaleString()}</div>
      </div>
      {diverges && !summary.fallback_applied ? (
        <div className="ml-auto rounded-md bg-yellow-100 px-2 py-1 text-xs text-yellow-900 dark:bg-yellow-950 dark:text-yellow-200">
          {t("pricing.batches.summary.diverges", {
            defaultValue:
              "Diverges from config fallback ({{fallback}} µ/cr). Consider updating config.",
            fallback: fallback.toLocaleString(),
          })}
        </div>
      ) : null}
    </div>
  );
}

// BatchesTable is the main list. Dense on purpose — the operator
// wants to eyeball outliers, not a marketing page. Retired rows
// (active=false) render at 50% opacity so the eye skips them.
function BatchesTable({
  rows,
  onEdit,
  onDelete,
}: {
  rows: PurchaseBatchView[];
  onEdit: (r: PurchaseBatchView) => void;
  onDelete: (id: string) => void;
}) {
  const { t } = useTranslation();
  if (rows.length === 0) {
    return (
      <div className="rounded-md border p-6 text-center text-sm text-muted-foreground">
        {t("pricing.batches.empty", {
          defaultValue: "No batches yet — click 'New batch' to add one.",
        })}
      </div>
    );
  }
  return (
    <div className="overflow-x-auto rounded-md border">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>
              {t("pricing.batches.col.date", { defaultValue: "Date" })}
            </TableHead>
            <TableHead>
              {t("pricing.batches.col.channel", { defaultValue: "Channel" })}
            </TableHead>
            <TableHead>
              {t("pricing.batches.col.seller", { defaultValue: "Seller" })}
            </TableHead>
            <TableHead>
              {t("pricing.batches.col.plan", { defaultValue: "Plan" })}
            </TableHead>
            <TableHead className="text-right">
              {t("pricing.batches.col.accounts", { defaultValue: "# acct" })}
            </TableHead>
            <TableHead className="text-right">
              {t("pricing.batches.col.credits", { defaultValue: "Credit ea" })}
            </TableHead>
            <TableHead className="text-right">
              {t("pricing.batches.col.paid", { defaultValue: "Paid $" })}
            </TableHead>
            <TableHead className="text-right">
              {t("pricing.batches.col.unitCost", {
                defaultValue: "Unit cost µ/cr",
              })}
            </TableHead>
            <TableHead>
              {t("pricing.batches.col.class", { defaultValue: "Class" })}
            </TableHead>
            <TableHead>
              {t("pricing.batches.col.promo", { defaultValue: "Promo" })}
            </TableHead>
            <TableHead>
              {t("pricing.batches.col.linked", { defaultValue: "Linked" })}
            </TableHead>
            <TableHead className="text-right">
              {t("pricing.batches.col.actions", { defaultValue: "Actions" })}
            </TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((r) => (
            <TableRow
              key={r.id}
              className={!r.active ? "opacity-50" : undefined}
            >
              <TableCell className="text-xs">
                {r.purchased_at.slice(0, 10)}
              </TableCell>
              <TableCell>
                <Badge variant="outline" className="text-[10px]">
                  {r.source_channel}
                </Badge>
              </TableCell>
              <TableCell className="text-xs">{r.source_seller || "—"}</TableCell>
              <TableCell className="text-xs font-mono">{r.plan_type}</TableCell>
              <TableCell className="text-right tabular-nums">
                {r.accounts_count}
              </TableCell>
              <TableCell className="text-right tabular-nums">
                {r.credits_per_account > 0 ? r.credits_per_account : "—"}
              </TableCell>
              <TableCell className="text-right tabular-nums">
                {formatUSD(r.total_paid_usd)}
              </TableCell>
              <TableCell className="text-right tabular-nums">
                {r.unit_cost_micros > 0
                  ? r.unit_cost_micros.toLocaleString()
                  : "—"}
              </TableCell>
              <TableCell>
                <PricingClassBadge cls={r.pricing_class} />
              </TableCell>
              <TableCell>
                <PromotionBadge promo={r.promotion_type} />
              </TableCell>
              <TableCell className="max-w-[200px] truncate text-[10px] text-muted-foreground">
                {r.linked_account_email || "—"}
              </TableCell>
              <TableCell className="text-right">
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => onEdit(r)}
                  title={t("common.edit", { defaultValue: "Edit" })}
                >
                  <IconEdit className="h-3 w-3" />
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => onDelete(r.id)}
                  title={t("common.delete", { defaultValue: "Delete" })}
                >
                  <IconTrash className="h-3 w-3" />
                </Button>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}

function PricingClassBadge({
  cls,
}: {
  cls: PurchaseBatchView["pricing_class"];
}) {
  if (cls === "normal") {
    return <span className="text-xs text-muted-foreground">normal</span>;
  }
  return (
    <Badge variant="secondary" className="text-[10px]">
      {cls}
    </Badge>
  );
}

// PromotionBadge renders the four promotion_type values. "none"
// (regular purchase) is muted to keep the eye on the promo rows,
// which are the ones the operator needs to reason about when
// eyeballing outliers vs baseline.
function PromotionBadge({
  promo,
}: {
  promo: PurchaseBatchView["promotion_type"];
}) {
  if (!promo || promo === "none") {
    return <span className="text-xs text-muted-foreground">—</span>;
  }
  return (
    <Badge variant="outline" className="text-[10px]">
      {promo}
    </Badge>
  );
}

// BatchDialog is the create + edit modal. Uses a single form for both;
// initial=null means Create, non-null means Edit. On Save it calls
// the appropriate mutation and closes.
function BatchDialog({
  initial,
  onClose,
  onSaved,
}: {
  initial: PurchaseBatchView | null;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { t } = useTranslation();
  const [form, setForm] = useState<PurchaseBatchInput>(
    initial
      ? {
          source_channel: initial.source_channel,
          source_seller: initial.source_seller,
          plan_type: initial.plan_type,
          accounts_count: initial.accounts_count,
          credits_per_account: initial.credits_per_account,
          total_paid_usd: initial.total_paid_usd,
          paid_currency: initial.paid_currency,
          pricing_class: initial.pricing_class,
          promotion_type: initial.promotion_type,
          active: initial.active,
          linked_account_email: initial.linked_account_email,
          rationale: initial.rationale,
          purchased_at: initial.purchased_at.slice(0, 10),
        }
      : {
          source_channel: "tg",
          source_seller: "",
          plan_type: "starter",
          accounts_count: 1,
          credits_per_account: 200,
          total_paid_usd: 0,
          paid_currency: "USD",
          pricing_class: "normal",
          promotion_type: "none",
          active: true,
          linked_account_email: "",
          rationale: "",
          purchased_at: new Date().toISOString().slice(0, 10),
        },
  );
  const [error, setError] = useState<string | null>(null);

  const save = useMutation({
    mutationFn: () =>
      initial
        ? admin.updatePurchaseBatch(initial.id, form)
        : admin.createPurchaseBatch(form),
    onSuccess: () => onSaved(),
    onError: (e: Error) => setError(e.message),
  });

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-w-2xl">
        <DialogHeader>
          <DialogTitle>
            {initial
              ? t("pricing.batches.dialog.editTitle", {
                  defaultValue: "Edit batch",
                })
              : t("pricing.batches.dialog.newTitle", {
                  defaultValue: "New batch",
                })}
          </DialogTitle>
          <DialogDescription>
            {t("pricing.batches.dialog.description", {
              defaultValue:
                "Purchase batches are historical facts — retire them via Active toggle instead of deleting.",
            })}
          </DialogDescription>
        </DialogHeader>
        <div className="grid grid-cols-2 gap-3">
          <FormField
            label={t("pricing.batches.form.date", {
              defaultValue: "Purchase date",
            })}
          >
            <Input
              type="date"
              value={form.purchased_at ?? ""}
              onChange={(e) =>
                setForm({ ...form, purchased_at: e.target.value })
              }
            />
          </FormField>
          <FormField
            label={t("pricing.batches.form.channel", {
              defaultValue: "Channel",
            })}
          >
            <Select
              value={form.source_channel ?? "tg"}
              onValueChange={(v) => setForm({ ...form, source_channel: v })}
            >
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {["tg", "taobao", "xianyu", "wechat", "official", "other"].map(
                  (c) => (
                    <SelectItem key={c} value={c}>
                      {c}
                    </SelectItem>
                  ),
                )}
              </SelectContent>
            </Select>
          </FormField>
          <FormField
            label={t("pricing.batches.form.seller", { defaultValue: "Seller" })}
          >
            <Input
              value={form.source_seller ?? ""}
              onChange={(e) =>
                setForm({ ...form, source_seller: e.target.value })
              }
              placeholder="BLACKHATWORLD"
            />
          </FormField>
          <FormField
            label={t("pricing.batches.form.plan", { defaultValue: "Plan" })}
          >
            <Select
              value={form.plan_type ?? "starter"}
              onValueChange={(v) => setForm({ ...form, plan_type: v })}
            >
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {["starter", "pro", "plus", "ultra"].map((p) => (
                  <SelectItem key={p} value={p}>
                    {p}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </FormField>
          <FormField
            label={t("pricing.batches.form.accountsCount", {
              defaultValue: "# accounts",
            })}
          >
            <Input
              type="number"
              min="1"
              value={form.accounts_count ?? 1}
              onChange={(e) =>
                setForm({ ...form, accounts_count: Number(e.target.value) })
              }
            />
          </FormField>
          <FormField
            label={t("pricing.batches.form.creditsPerAccount", {
              defaultValue: "Credits per account",
            })}
          >
            <Input
              type="number"
              min="0"
              step="0.01"
              value={form.credits_per_account ?? 0}
              onChange={(e) =>
                setForm({
                  ...form,
                  credits_per_account: Number(e.target.value),
                })
              }
            />
          </FormField>
          <FormField
            label={t("pricing.batches.form.paid", {
              defaultValue: "Total paid (USD)",
            })}
          >
            <Input
              type="number"
              min="0"
              step="0.01"
              value={form.total_paid_usd ?? 0}
              onChange={(e) =>
                setForm({ ...form, total_paid_usd: Number(e.target.value) })
              }
            />
          </FormField>
          <FormField
            label={t("pricing.batches.form.class", {
              defaultValue: "Pricing class",
            })}
          >
            <Select
              value={form.pricing_class ?? "normal"}
              onValueChange={(v) =>
                setForm({
                  ...form,
                  pricing_class:
                    v as PurchaseBatchInput["pricing_class"],
                })
              }
            >
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {(["normal", "activity", "bug", "promo"] as const).map((c) => (
                  <SelectItem key={c} value={c}>
                    {c}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </FormField>
          <FormField
            label={t("pricing.batches.form.promotion", {
              defaultValue: "Promotion type",
            })}
          >
            <Select
              value={form.promotion_type ?? "none"}
              onValueChange={(v) =>
                setForm({
                  ...form,
                  promotion_type:
                    v as PurchaseBatchInput["promotion_type"],
                })
              }
            >
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {(
                  [
                    "none",
                    "first_signup",
                    "unlim_1day",
                    "standard_credit_boost",
                  ] as const
                ).map((p) => (
                  <SelectItem key={p} value={p}>
                    {p}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </FormField>
          <FormField
            label={t("pricing.batches.form.linked", {
              defaultValue: "Linked account email",
            })}
            colSpan={2}
          >
            <Input
              value={form.linked_account_email ?? ""}
              onChange={(e) =>
                setForm({ ...form, linked_account_email: e.target.value })
              }
              placeholder="user@example.com"
            />
          </FormField>
          <FormField
            label={t("pricing.batches.form.rationale", {
              defaultValue: "Notes / rationale",
            })}
            colSpan={2}
          >
            <Input
              value={form.rationale ?? ""}
              onChange={(e) => setForm({ ...form, rationale: e.target.value })}
              placeholder={t("pricing.batches.form.rationalePlaceholder", {
                defaultValue: "Why is this batch worth remembering?",
              })}
            />
          </FormField>
          <FormField
            label={t("pricing.batches.form.active", { defaultValue: "Active" })}
            colSpan={2}
          >
            <div className="flex items-center gap-2">
              <Switch
                checked={form.active ?? true}
                onCheckedChange={(v) => setForm({ ...form, active: v })}
              />
              <span className="text-xs text-muted-foreground">
                {t("pricing.batches.form.activeHelp", {
                  defaultValue:
                    "Inactive batches keep audit trail but are excluded from the weighted average.",
                })}
              </span>
            </div>
          </FormField>
        </div>
        {error ? (
          <div className="rounded-md border border-destructive/30 bg-destructive/10 p-2 text-xs text-destructive">
            {error}
          </div>
        ) : null}
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            {t("common.cancel", { defaultValue: "Cancel" })}
          </Button>
          <Button onClick={() => save.mutate()} disabled={save.isPending}>
            {t("common.save", { defaultValue: "Save" })}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function FormField({
  label,
  children,
  colSpan,
}: {
  label: string;
  children: React.ReactNode;
  colSpan?: 1 | 2;
}) {
  return (
    <div className={colSpan === 2 ? "col-span-2 space-y-1" : "space-y-1"}>
      <Label className="text-xs">{label}</Label>
      {children}
    </div>
  );
}

export const pricingPurchaseBatchesRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/pricing/purchase-batches",
  staticData: { titleKey: "nav.pricingBatches" },
  component: PricingPurchaseBatchesPage,
});
