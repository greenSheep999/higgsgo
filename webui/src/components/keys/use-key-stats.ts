import { useQuery } from "@tanstack/react-query";
import { admin } from "@/lib/api";

// useKeyStats aggregates usage_events over the last 30 days by
// (api_key_id, status) in a single request and returns a Map keyed by
// api_key_id. Per-key stats are cheaper computed on the client after
// this one fetch than issuing N separate /keys/{id}/stats requests.

export interface KeyStatsRow {
  requests: number;
  completed: number;
  failed: number;
  refunded: number;
  // Aligned with the account card resources:
  //   chargedCreditsH  = subscription pool spend (what was actually
  //                      billed to the caller; drives revenue)
  //   freeCreditsH     = free / unlim / flex-unlim spend the account
  //                      absorbed on the caller's behalf (= actual - charged)
  //   totalCreditsH    = sum, for reference / eventual "extra pool"
  //                      breakdown when metering gains that split
  chargedCreditsH: number;
  totalCreditsH: number;
  freeCreditsH: number;
}

export function useKeyStats() {
  // No since — sum the entire history so the Keys list shows lifetime
  // totals. Time-scoped analysis lives on the Usage page instead.
  const q = useQuery({
    queryKey: ["admin", "usage", "keys-total"],
    queryFn: () =>
      admin.aggregateUsage({
        group_by: ["api_key_id", "status"],
      }),
    staleTime: 30_000,
    refetchInterval: 60_000,
  });

  const map = new Map<string, KeyStatsRow>();
  (q.data ?? []).forEach((row) => {
    const id = row.keys.api_key_id ?? "";
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
    // Free = whatever the account absorbed on the caller's behalf
    // (unlim / flex-unlim / anything the caller didn't pay for).
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
