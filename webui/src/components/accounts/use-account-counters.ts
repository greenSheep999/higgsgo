import { useQuery } from "@tanstack/react-query";
import { admin } from "@/lib/api";

// useAccountCounters fetches cumulative successes / failures per account
// over a rolling window by grouping /admin/usage/aggregate on both
// account_id and status. One HTTP call for the whole list — much cheaper
// than N per-row lookups. Default window is 30 days: that's long enough
// to reflect an account's real reliability but short enough that a
// recovered account isn't dragged forever by ancient failures.
//
// Returns a lookup keyed by account_id. Missing accounts (no usage in
// the window) return `undefined`, which the card renders as "0".

export interface AccountCounters {
  successes: number;
  failures: number;
}

export function useAccountCounters(windowDays = 30) {
  const since = new Date(
    Date.now() - windowDays * 24 * 3600_000,
  ).toISOString();

  const q = useQuery({
    queryKey: ["admin", "usage", "counters", "byAccount", windowDays],
    queryFn: () =>
      admin.aggregateUsage({
        since,
        group_by: ["account_id", "status"],
      }),
    refetchInterval: 60_000,
    staleTime: 30_000,
  });

  const map = new Map<string, AccountCounters>();
  (q.data ?? []).forEach((row) => {
    const id = row.keys.account_id ?? "";
    if (!id) return;
    const prev = map.get(id) ?? { successes: 0, failures: 0 };
    // usage_events aggregate carries status split even though we also
    // group by status — request_count for that (account, status) tuple
    // is precisely the count we want per bucket.
    const status = row.keys.status ?? "";
    if (status === "completed") prev.successes += row.request_count;
    else if (status === "failed") prev.failures += row.request_count;
    map.set(id, prev);
  });

  return { map, isLoading: q.isLoading };
}
