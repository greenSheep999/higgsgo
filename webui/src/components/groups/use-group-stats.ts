import { useQueries, useQuery } from "@tanstack/react-query";
import { admin } from "@/lib/api";

// useGroupStats mirrors useKeyStats: aggregate lifetime usage across
// every group in one /admin/usage/aggregate call and expose a map so
// each Groups row can render its request / charged / unlimited totals
// without fanning out. Same field semantics as the Keys page.

export interface GroupStatsRow {
  requests: number;
  completed: number;
  failed: number;
  refunded: number;
  chargedCreditsH: number;
  totalCreditsH: number;
  freeCreditsH: number;
}

export function useGroupStats() {
  const q = useQuery({
    queryKey: ["admin", "usage", "groups-total"],
    queryFn: () =>
      admin.aggregateUsage({
        group_by: ["group_id", "status"],
      }),
    staleTime: 30_000,
    refetchInterval: 60_000,
  });

  const map = new Map<string, GroupStatsRow>();
  (q.data ?? []).forEach((row) => {
    const id = row.keys.group_id ?? "";
    if (!id) return;
    const prev =
      map.get(id) ?? {
        requests: 0,
        completed: 0,
        failed: 0,
        refunded: 0,
        chargedCreditsH: 0,
        totalCreditsH: 0,
        freeCreditsH: 0,
      };
    prev.requests += row.request_count;
    prev.chargedCreditsH += row.charged_credits_h;
    prev.totalCreditsH += row.total_credits_h;
    prev.freeCreditsH += Math.max(
      0,
      row.total_credits_h - row.charged_credits_h,
    );
    const status = row.keys.status ?? "";
    if (status === "completed") prev.completed += row.request_count;
    else if (status === "failed") prev.failed += row.request_count;
    else if (status === "refunded") prev.refunded += row.request_count;
    map.set(id, prev);
  });

  return { map, isLoading: q.isLoading };
}

// useGroupMemberCounts fans out /admin/groups/{id}/members and
// /admin/groups/{id}/bindings so the Groups list can render the
// account / key counts without opening each detail page. Pool sizes
// we target are small (< 50 groups); the fan-out cost is fine.
export interface GroupCountsRow {
  accounts: number;
  keys: number;
}

export function useGroupCounts(groupIds: string[]) {
  const memberQs = useQueries({
    queries: groupIds.map((id) => ({
      queryKey: ["admin", "groups", id, "members"],
      queryFn: () => admin.listGroupMembers(id),
      staleTime: 20_000,
    })),
  });
  // Namespace the queryKey so this shape ({id, keys}) doesn't collide
  // with the useKeyGroups ({group, keys}) or group-detail (string[])
  // callers that hit the same endpoint.
  const bindingQs = useQueries({
    queries: groupIds.map((id) => ({
      queryKey: ["admin", "groups", id, "bindings", "groups-view"],
      queryFn: async () => {
        const bearer = localStorage.getItem("higgsgo.adminBearer") ?? "";
        const r = await fetch(`/admin/groups/${id}/bindings`, {
          headers: { Authorization: `Bearer ${bearer}` },
        });
        const j = (await r.json()) as { data?: string[] };
        return { id, keys: j.data ?? [] };
      },
      staleTime: 20_000,
    })),
  });

  const map = new Map<string, GroupCountsRow>();
  groupIds.forEach((id, i) => {
    map.set(id, {
      accounts: memberQs[i]?.data?.members?.length ?? 0,
      keys: bindingQs[i]?.data?.keys?.length ?? 0,
    });
  });

  return {
    map,
    isLoading:
      memberQs.some((q) => q.isLoading) || bindingQs.some((q) => q.isLoading),
  };
}
