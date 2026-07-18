import { useQueries, useQuery } from "@tanstack/react-query";
import { admin, type Group } from "@/lib/api";

// useKeyGroups mirrors useAccountGroups (in accounts/) — reverse-indexes
// /admin/groups/{id}/bindings into an apiKeyId -> [{id,name}] map so
// the Keys list can render group badges without fanning out one query
// per row.

export type KeyGroupIndex = Map<string, { id: string; name: string }[]>;

export function useKeyGroups(): {
  index: KeyGroupIndex;
  isLoading: boolean;
} {
  const groups = useQuery({
    queryKey: ["admin", "groups"],
    queryFn: admin.listGroups,
  });

  const bindingQueries = useQueries({
    queries: (groups.data ?? []).map((g: Group) => ({
      queryKey: ["admin", "groups", g.id, "bindings"],
      queryFn: async () => {
        const r = await fetch(`/admin/groups/${g.id}/bindings`, {
          headers: {
            Authorization: `Bearer ${localStorage.getItem("higgsgo.adminBearer") ?? ""}`,
          },
        });
        const j = (await r.json()) as { data?: string[] };
        return { group: g, keys: j.data ?? [] };
      },
      staleTime: 15_000,
    })),
  });

  const index: KeyGroupIndex = new Map();
  bindingQueries.forEach((q) => {
    if (!q.data) return;
    q.data.keys.forEach((keyId) => {
      const prev = index.get(keyId) ?? [];
      prev.push({ id: q.data.group.id, name: q.data.group.name });
      index.set(keyId, prev);
    });
  });

  return { index, isLoading: groups.isLoading || bindingQueries.some((q) => q.isLoading) };
}
