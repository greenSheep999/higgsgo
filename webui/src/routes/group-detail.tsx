import { useState } from "react";
import { createRoute, Link, useParams } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { useMutation, useQueries, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import {
  IconArrowLeft,
  IconPlus,
  IconTrash,
  IconUsersGroup,
} from "@tabler/icons-react";

import { admin, ApiError, type Account, type ApiKey } from "@/lib/api";
import { formatCredits, formatRelative } from "@/lib/format";
import { presetWindow } from "@/lib/time-window";
import { rootRoute } from "@/routes/root";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import { Progress } from "@/components/ui/progress";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  Tabs,
  TabsContent,
  TabsList,
  TabsTrigger,
} from "@/components/ui/tabs";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { StatusBadge, accountStatusTone, keyStatusTone } from "@/components/ui/status-badge";

// Group detail page — four tabs (info / members / keys / usage). The
// group is the pool's routing + quota edge, so this page has to
// answer three practical questions: which accounts are eligible to
// serve traffic under it, which keys can send that traffic, and how
// close we are to the monthly budget cap.

function GroupDetail() {
  const { t } = useTranslation();
  const { id } = useParams({ from: groupDetailRoute.id });
  const qc = useQueryClient();

  const groupQ = useQuery({
    queryKey: ["admin", "groups", id],
    queryFn: async () => {
      const list = await admin.listGroups();
      return list.find((g) => g.id === id) ?? null;
    },
  });

  const membersQ = useQuery({
    queryKey: ["admin", "groups", id, "members"],
    queryFn: () => admin.listGroupMembers(id),
  });

  // Namespaced queryKey: the same /admin/groups/{id}/bindings endpoint
  // is consumed by useKeyGroups and useGroupCounts too, each with a
  // different return shape. A bare shared key silently corrupts caches
  // across pages, so each caller tags its own view.
  const bindingsQ = useQuery({
    queryKey: ["admin", "groups", id, "bindings", "detail-view"],
    queryFn: async () => {
      const bearer = localStorage.getItem("higgsgo.adminBearer") ?? "";
      const r = await fetch(`/admin/groups/${id}/bindings`, {
        headers: { Authorization: `Bearer ${bearer}` },
      });
      const j = (await r.json()) as { data?: string[] };
      return j.data ?? [];
    },
  });

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ["admin", "groups", id] });
    qc.invalidateQueries({ queryKey: ["admin", "groups"] });
  };

  const g = groupQ.data;
  const budgetPct =
    g && g.monthly_credit_budget > 0
      ? Math.min(100, (g.monthly_credit_used / g.monthly_credit_budget) * 100)
      : 0;

  return (
    <div className="flex flex-col gap-4">
      <div className="flex items-center gap-2">
        <Button variant="ghost" size="sm" asChild>
          <Link to="/groups">
            <IconArrowLeft /> {t("common.back", { defaultValue: "Back" })}
          </Link>
        </Button>
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <IconUsersGroup className="size-5" />
            {groupQ.isLoading ? <Skeleton className="h-5 w-32" /> : g?.name}
          </CardTitle>
          <CardDescription className="font-mono text-xs">{id}</CardDescription>
        </CardHeader>
        <CardContent>
          {g && g.monthly_credit_budget > 0 ? (
            <div className="space-y-1">
              <div className="flex items-baseline justify-between text-xs">
                <span className="text-muted-foreground">
                  {t("groups.budgetProgress", {
                    used: formatCredits(g.monthly_credit_used),
                    budget: formatCredits(g.monthly_credit_budget),
                  })}
                </span>
                <span className="tabular-nums">{budgetPct.toFixed(0)}%</span>
              </div>
              <Progress value={budgetPct} className="h-1.5" />
            </div>
          ) : (
            <StatusBadge tone="brand">{t("groups.noBudget")}</StatusBadge>
          )}
        </CardContent>
      </Card>

      <Tabs defaultValue="info">
        <TabsList>
          <TabsTrigger value="info">Info</TabsTrigger>
          <TabsTrigger value="members">
            Accounts{" "}
            {membersQ.data ? `(${membersQ.data.members.length})` : ""}
          </TabsTrigger>
          <TabsTrigger value="keys">
            Keys{" "}
            {bindingsQ.data ? `(${bindingsQ.data.length})` : ""}
          </TabsTrigger>
          <TabsTrigger value="usage">Usage</TabsTrigger>
        </TabsList>

        <TabsContent value="info" className="pt-4">
          {g ? <InfoTab group={g} onUpdated={invalidate} /> : null}
        </TabsContent>
        <TabsContent value="members" className="pt-4">
          <MembersTab
            groupId={id}
            memberIds={membersQ.data?.members ?? []}
            loading={membersQ.isLoading}
            onChanged={() =>
              qc.invalidateQueries({
                queryKey: ["admin", "groups", id, "members"],
              })
            }
          />
        </TabsContent>
        <TabsContent value="keys" className="pt-4">
          <KeysTab
            groupId={id}
            boundIds={bindingsQ.data ?? []}
            loading={bindingsQ.isLoading}
            onChanged={() =>
              qc.invalidateQueries({
                queryKey: ["admin", "groups", id, "bindings"],
              })
            }
          />
        </TabsContent>
        <TabsContent value="usage" className="pt-4">
          <UsageTab groupId={id} />
        </TabsContent>
      </Tabs>
    </div>
  );
}

// InfoTab surfaces the mutable group fields as a small form and
// PUT-backs when the operator hits save. Description / route strategy
// / concurrency caps / monthly budget stay editable here.
function InfoTab({
  group,
  onUpdated,
}: {
  group: NonNullable<Awaited<ReturnType<typeof admin.listGroups>>>[number];
  onUpdated: () => void;
}) {
  const { t } = useTranslation();
  const [name, setName] = useState(group.name);
  const [description, setDescription] = useState(group.description);
  // Empty string is the "unlimited" sentinel for the three numeric
  // caps; the placeholder shows "无限制" instead of a bare 0.
  const [maxJobs, setMaxJobs] = useState(
    group.max_concurrent_jobs > 0 ? String(group.max_concurrent_jobs) : "",
  );
  const [maxPerAcct, setMaxPerAcct] = useState(
    group.max_concurrent_per_account > 0
      ? String(group.max_concurrent_per_account)
      : "",
  );
  const [budget, setBudget] = useState(
    group.monthly_credit_budget > 0
      ? String(group.monthly_credit_budget / 100)
      : "",
  );
  const [route, setRoute] = useState(group.route_strategy);

  const update = useMutation({
    mutationFn: () =>
      admin.updateGroup(group.id, {
        name: name.trim(),
        description: description,
        max_concurrent_jobs: parseInt(maxJobs || "0", 10),
        max_concurrent_per_account: parseInt(maxPerAcct || "0", 10),
        monthly_credit_budget: Math.round(parseFloat(budget || "0") * 100),
        route_strategy: route,
      }),
    onSuccess: () => {
      toast.success(t("groups.toasts.updated", { name }));
      onUpdated();
    },
    onError: (err) =>
      toast.error(
        err instanceof ApiError
          ? `${err.status} ${err.type}: ${err.message}`
          : err instanceof Error
            ? err.message
            : "update failed",
      ),
  });

  return (
    <div className="grid gap-4 md:grid-cols-2">
      <div className="grid gap-2">
        <Label htmlFor="g-name">{t("groups.form.name")}</Label>
        <Input id="g-name" value={name} onChange={(e) => setName(e.target.value)} />
      </div>
      <div className="grid gap-2">
        <Label htmlFor="g-route">{t("groups.form.routeStrategy")}</Label>
        <Select value={route} onValueChange={setRoute}>
          <SelectTrigger>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="round_robin">round_robin</SelectItem>
            <SelectItem value="least_used">least_used</SelectItem>
            <SelectItem value="priority">priority</SelectItem>
          </SelectContent>
        </Select>
      </div>
      <div className="md:col-span-2 grid gap-2">
        <Label htmlFor="g-desc">{t("groups.form.description")}</Label>
        <Input id="g-desc" value={description} onChange={(e) => setDescription(e.target.value)} />
      </div>
      <div className="grid gap-2">
        <Label htmlFor="g-max">{t("groups.form.maxConcurrent")}</Label>
        <Input
          id="g-max"
          type="number"
          min={0}
          placeholder={t("common.unlimited")}
          value={maxJobs}
          onChange={(e) => setMaxJobs(e.target.value)}
        />
      </div>
      <div className="grid gap-2">
        <Label htmlFor="g-max-per">{t("groups.form.maxConcurrentPerAccount")}</Label>
        <Input
          id="g-max-per"
          type="number"
          min={0}
          placeholder={t("common.unlimited")}
          value={maxPerAcct}
          onChange={(e) => setMaxPerAcct(e.target.value)}
        />
      </div>
      <div className="grid gap-2 md:col-span-2">
        <Label htmlFor="g-budget">{t("groups.form.monthlyBudget")}</Label>
        <Input
          id="g-budget"
          type="number"
          min={0}
          step="0.01"
          placeholder={t("common.unlimited")}
          value={budget}
          onChange={(e) => setBudget(e.target.value)}
        />
        <p className="text-xs text-muted-foreground">
          {t("groups.form.monthlyBudgetHint")}
        </p>
      </div>
      <div className="md:col-span-2">
        <Button onClick={() => update.mutate()} disabled={update.isPending}>
          {update.isPending ? t("common.loading") : t("common.save")}
        </Button>
      </div>
    </div>
  );
}

// MembersTab lists the accounts this group serves. Add/remove links
// straight to the store's AddMember/RemoveMember endpoints; we
// side-load the full accounts list so the picker can be by email.
function MembersTab({
  groupId,
  memberIds,
  loading,
  onChanged,
}: {
  groupId: string;
  memberIds: string[];
  loading: boolean;
  onChanged: () => void;
}) {
  const accountsQ = useQuery({
    queryKey: ["admin", "accounts", "for-groups"],
    queryFn: () => admin.listAccounts({}),
  });
  const memberSet = new Set(memberIds);
  const members: Account[] = (accountsQ.data ?? []).filter((a) => memberSet.has(a.id));
  const candidates: Account[] = (accountsQ.data ?? []).filter((a) => !memberSet.has(a.id));

  const [pick, setPick] = useState<string>("");

  const add = useMutation({
    mutationFn: async () => {
      const bearer = localStorage.getItem("higgsgo.adminBearer") ?? "";
      const r = await fetch(`/admin/groups/${groupId}/members`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Authorization: `Bearer ${bearer}`,
        },
        body: JSON.stringify({ account_id: pick }),
      });
      if (!r.ok) throw new Error(`HTTP ${r.status}`);
    },
    onSuccess: () => {
      toast.success("added");
      setPick("");
      onChanged();
    },
    onError: (err) =>
      toast.error(err instanceof Error ? err.message : String(err)),
  });

  const remove = useMutation({
    mutationFn: async (accountId: string) => {
      const bearer = localStorage.getItem("higgsgo.adminBearer") ?? "";
      const r = await fetch(
        `/admin/groups/${groupId}/members/${accountId}`,
        {
          method: "DELETE",
          headers: { Authorization: `Bearer ${bearer}` },
        },
      );
      if (!r.ok) throw new Error(`HTTP ${r.status}`);
    },
    onSuccess: () => {
      toast.success("removed");
      onChanged();
    },
    onError: (err) =>
      toast.error(err instanceof Error ? err.message : String(err)),
  });

  return (
    <div className="space-y-3">
      <div className="flex items-center gap-2">
        <Select value={pick} onValueChange={setPick}>
          <SelectTrigger className="w-72">
            <SelectValue placeholder="Add account…" />
          </SelectTrigger>
          <SelectContent>
            {candidates.map((a) => (
              <SelectItem key={a.id} value={a.id}>
                {a.email} · {a.plan_type}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Button
          size="sm"
          onClick={() => add.mutate()}
          disabled={!pick || add.isPending}
        >
          <IconPlus />
          Add
        </Button>
      </div>
      {loading || accountsQ.isLoading ? (
        <Skeleton className="h-24 w-full" />
      ) : (
        <div className="overflow-hidden rounded-md border">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Email</TableHead>
                <TableHead>Plan</TableHead>
                <TableHead>Status</TableHead>
                <TableHead className="w-10" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {members.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={4} className="text-center text-sm text-muted-foreground">
                    No accounts bound
                  </TableCell>
                </TableRow>
              ) : (
                members.map((a) => (
                  <TableRow key={a.id}>
                    <TableCell>
                      <Link
                        to="/accounts"
                        className="hover:underline"
                      >
                        {a.email}
                      </Link>
                      <div className="font-mono text-[10px] text-muted-foreground">
                        {a.id}
                      </div>
                    </TableCell>
                    <TableCell>{a.plan_type}</TableCell>
                    <TableCell>
                      <StatusBadge tone={accountStatusTone(a.status)}>
                        {a.status}
                      </StatusBadge>
                    </TableCell>
                    <TableCell>
                      <Button
                        variant="ghost"
                        size="icon"
                        className="size-8"
                        onClick={() => remove.mutate(a.id)}
                      >
                        <IconTrash className="size-4" />
                      </Button>
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </div>
      )}
    </div>
  );
}

// KeysTab is the mirror of MembersTab but for api_keys. Bindings run
// through /admin/groups/{gid}/bindings — the same M:N table the Keys
// list consults from the other direction.
function KeysTab({
  groupId,
  boundIds,
  loading,
  onChanged,
}: {
  groupId: string;
  boundIds: string[];
  loading: boolean;
  onChanged: () => void;
}) {
  const keysQ = useQuery({
    queryKey: ["admin", "keys"],
    queryFn: admin.listKeys,
  });
  const boundSet = new Set(boundIds);
  const bound: ApiKey[] = (keysQ.data ?? []).filter((k) => boundSet.has(k.id));
  const candidates: ApiKey[] = (keysQ.data ?? []).filter((k) => !boundSet.has(k.id));
  const [pick, setPick] = useState<string>("");

  const bind = useMutation({
    mutationFn: () => admin.bindKeyToGroup(groupId, pick),
    onSuccess: () => {
      toast.success("bound");
      setPick("");
      onChanged();
    },
    onError: (err) =>
      toast.error(err instanceof Error ? err.message : String(err)),
  });

  const unbind = useMutation({
    mutationFn: (id: string) => admin.unbindKeyFromGroup(groupId, id),
    onSuccess: () => {
      toast.success("unbound");
      onChanged();
    },
    onError: (err) =>
      toast.error(err instanceof Error ? err.message : String(err)),
  });

  return (
    <div className="space-y-3">
      <div className="flex items-center gap-2">
        <Select value={pick} onValueChange={setPick}>
          <SelectTrigger className="w-72">
            <SelectValue placeholder="Bind api key…" />
          </SelectTrigger>
          <SelectContent>
            {candidates.map((k) => (
              <SelectItem key={k.id} value={k.id}>
                {k.name}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Button size="sm" onClick={() => bind.mutate()} disabled={!pick || bind.isPending}>
          <IconPlus />
          Bind
        </Button>
      </div>
      {loading || keysQ.isLoading ? (
        <Skeleton className="h-24 w-full" />
      ) : (
        <div className="overflow-hidden rounded-md border">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Status</TableHead>
                <TableHead className="text-right">Monthly used</TableHead>
                <TableHead>Last used</TableHead>
                <TableHead className="w-10" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {bound.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={5} className="text-center text-sm text-muted-foreground">
                    No keys bound
                  </TableCell>
                </TableRow>
              ) : (
                bound.map((k) => (
                  <TableRow key={k.id}>
                    <TableCell>
                      {k.name}
                      <div className="font-mono text-[10px] text-muted-foreground">
                        {k.id}
                      </div>
                    </TableCell>
                    <TableCell>
                      <StatusBadge tone={keyStatusTone(k.status)}>{k.status}</StatusBadge>
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {formatCredits(k.monthly_used)}
                    </TableCell>
                    <TableCell className="text-muted-foreground">
                      {formatRelative(k.last_used_at)}
                    </TableCell>
                    <TableCell>
                      <Button
                        variant="ghost"
                        size="icon"
                        className="size-8"
                        onClick={() => unbind.mutate(k.id)}
                      >
                        <IconTrash className="size-4" />
                      </Button>
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </div>
      )}
    </div>
  );
}

// UsageTab hits /admin/usage/aggregate scoped by group_id. Two rollups
// side-by-side: by model and by key, both sorted by charged credits.
function UsageTab({ groupId }: { groupId: string }) {
  const window = presetWindow("30d");
  const queries = useQueries({
    queries: [
      {
        queryKey: ["admin", "usage", "group", groupId, "byModel"],
        queryFn: () =>
          admin.aggregateUsage({
            since: window.since,
            until: window.until,
            group_by: ["model_alias"],
            group_id: groupId,
          }),
      },
      {
        queryKey: ["admin", "usage", "group", groupId, "byKey"],
        queryFn: () =>
          admin.aggregateUsage({
            since: window.since,
            until: window.until,
            group_by: ["api_key_id"],
            group_id: groupId,
          }),
      },
    ],
  });
  const [byModel, byKey] = queries;

  return (
    <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
      <UsageRollup title="By model" dim="model_alias" q={byModel} />
      <UsageRollup title="By api key" dim="api_key_id" q={byKey} />
    </div>
  );
}

function UsageRollup({
  title,
  dim,
  q,
}: {
  title: string;
  dim: "model_alias" | "api_key_id";
  q: { data?: Awaited<ReturnType<typeof admin.aggregateUsage>>; isLoading: boolean };
}) {
  const rows = (q.data ?? [])
    .filter((r) => (r.keys[dim] ?? "") !== "")
    .sort((a, b) => b.charged_credits_h - a.charged_credits_h)
    .slice(0, 10);
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">{title}</CardTitle>
      </CardHeader>
      <CardContent>
        {q.isLoading ? (
          <Skeleton className="h-24 w-full" />
        ) : rows.length === 0 ? (
          <p className="text-sm text-muted-foreground">nothing in 30d</p>
        ) : (
          <ul className="space-y-1 text-xs">
            {rows.map((r, i) => (
              <li key={i} className="flex items-baseline justify-between">
                <span className="min-w-0 truncate font-mono">{r.keys[dim]}</span>
                <span className="tabular-nums text-muted-foreground">
                  {r.request_count} · {formatCredits(r.charged_credits_h)}
                </span>
              </li>
            ))}
          </ul>
        )}
      </CardContent>
    </Card>
  );
}

export const groupDetailRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/groups/$id",
  staticData: { titleKey: "nav.groups" },
  component: GroupDetail,
});
