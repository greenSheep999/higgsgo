import { useQueries, useQuery } from "@tanstack/react-query";
import { admin, type Group } from "@/lib/api";

// useAccountGroups reverse-indexes /admin/groups/{id}/members so a card
// can show which groups an account currently belongs to. The backend
// exposes group→members but not account→groups, so we fan out one
// request per group and join client-side. Pool sizes we care about
// have < ~20 groups; even with per-request cost the join stays fast.
//
// Returns a Map<accountId, {id,name}[]> that the card grid indexes into.
// While any group's members query is loading, the map is still returned
// with whatever is ready — the card just shows fewer badges until the
// missing ones settle.

export type AccountGroupIndex = Map<string, { id: string; name: string }[]>;

export function useAccountGroups(): {
  index: AccountGroupIndex;
  isLoading: boolean;
} {
  const groups = useQuery({
    queryKey: ["admin", "groups"],
    queryFn: admin.listGroups,
  });

  const memberQueries = useQueries({
    queries: (groups.data ?? []).map((g: Group) => ({
      queryKey: ["admin", "groups", g.id, "members"],
      queryFn: () => admin.listGroupMembers(g.id),
      staleTime: 15_000,
    })),
  });

  const index: AccountGroupIndex = new Map();
  (groups.data ?? []).forEach((g: Group, i: number) => {
    const q = memberQueries[i];
    if (!q?.data) return;
    q.data.members.forEach((accountId) => {
      const prev = index.get(accountId) ?? [];
      prev.push({ id: g.id, name: g.name });
      index.set(accountId, prev);
    });
  });

  const isLoading =
    groups.isLoading || memberQueries.some((q) => q.isLoading);
  return { index, isLoading };
}
