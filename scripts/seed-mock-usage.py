#!/usr/bin/env python3
"""Seed the dev SQLite DB with mock jobs + usage_events so the WebUI
dashboard / jobs / usage pages have something realistic to render.

DEV ONLY. Reads the existing accounts / api_keys / account_groups from the
target DB and hangs synthetic jobs+usage off them so all foreign keys hold.
Rows are spread across the last N days with a realistic mix of models,
media types, statuses, latency and credits.

Usage:
    python3 scripts/seed-mock-usage.py [--db data/higgsgo.db] [--days 14]
                                       [--per-day 40] [--wipe]

--wipe removes ONLY the synthetic rows (jobs / usage_events / usage_daily_agg)
before seeding, so real proxied traffic is never touched (there is none in
dev anyway, but keep it safe).
"""

import argparse
import json
import random
import sqlite3
import string
import sys
from datetime import datetime, timedelta, timezone

# (alias, jst, media_type, est_cost_hundredths) — a spread of real catalog
# entries across image / video so the model breakdown looks alive.
MODELS = [
    ("nano-banana", "nano_banana", "image", 100),
    ("nano-banana-2", "nano_banana_2", "image", 200),
    ("gpt-image", "gpt_image", "image", 300),
    ("flux-2", "flux_2", "image", 150),
    ("seedream-4", "seedream_4", "image", 250),
    ("kling-omni-image", "kling_omni_image", "image", 50),
    ("seedance-2-0", "seedance_2_0", "video", 1800),
    ("kling-3", "kling_3", "video", 1000),
    ("veo3-1-fast", "veo3_1_fast", "video", 2400),
    ("grok-video", "grok_video", "video", 750),
]

# status → weight. Mostly completed, some failed, a few refunded/timeout.
STATUS_WEIGHTS = [
    ("completed", 76),
    ("failed", 12),
    ("refunded", 6),
    ("timeout", 4),
    ("in_progress", 2),
]


def rid(prefix: str, n: int = 20) -> str:
    return prefix + "_" + "".join(
        random.choices(string.ascii_letters + string.digits, k=n)
    )


def weighted_status() -> str:
    pool = []
    for s, w in STATUS_WEIGHTS:
        pool.extend([s] * w)
    return random.choice(pool)


def load_refs(cx: sqlite3.Connection):
    accts = [r[0] for r in cx.execute(
        "SELECT id FROM accounts WHERE status='active'").fetchall()]
    if not accts:
        accts = [r[0] for r in cx.execute("SELECT id FROM accounts").fetchall()]
    keys = cx.execute(
        "SELECT id, markup_pct FROM api_keys").fetchall()
    groups = [r[0] for r in cx.execute(
        "SELECT id FROM account_groups").fetchall()]
    return accts, keys, groups


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--db", default="data/higgsgo.db")
    ap.add_argument("--days", type=int, default=14)
    ap.add_argument("--per-day", type=int, default=40)
    ap.add_argument("--wipe", action="store_true")
    args = ap.parse_args()

    cx = sqlite3.connect(args.db)
    cx.execute("PRAGMA foreign_keys = ON;")

    accts, keys, groups = load_refs(cx)
    if not accts or not keys:
        print("need at least one account and one api_key in the DB first",
              file=sys.stderr)
        return 1

    if args.wipe:
        cx.execute("DELETE FROM usage_events")
        cx.execute("DELETE FROM jobs")
        cx.execute("DELETE FROM usage_daily_agg")
        cx.commit()
        print("wiped jobs / usage_events / usage_daily_agg")

    now = datetime.now(timezone.utc)
    n_jobs = 0
    n_events = 0

    for day in range(args.days):
        # More traffic on recent days so the trend chart slopes.
        day_count = int(args.per_day * (1.0 - 0.4 * day / max(args.days, 1)))
        for _ in range(max(day_count, 1)):
            alias, jst, media, est = random.choice(MODELS)
            key_id, markup = random.choice(keys)
            group_id = random.choice(groups) if groups else None
            acct = random.choice(accts)
            status = weighted_status()

            ts = now - timedelta(
                days=day,
                hours=random.randint(0, 23),
                minutes=random.randint(0, 59),
            )
            ts_s = ts.strftime("%Y-%m-%dT%H:%M:%SZ")

            # cost model: actual ≈ est with jitter; charged = actual*markup,
            # zero for failed/refunded terminals.
            actual = int(est * random.uniform(0.85, 1.2))
            if status in ("failed", "timeout", "in_progress"):
                actual, charged = 0, 0
            elif status == "refunded":
                charged = 0
            else:
                charged = int(actual * markup)

            latency = (random.randint(1500, 9000) if media == "image"
                       else random.randint(8000, 90000))
            if status == "in_progress":
                latency, finished = None, None
            else:
                finished = (ts + timedelta(milliseconds=latency or 0)) \
                    .strftime("%Y-%m-%dT%H:%M:%SZ")
            polls = random.randint(1, 6) if status != "in_progress" else 0
            refunded = 1 if status == "refunded" else 0
            err_type = None
            if status == "failed":
                err_type = random.choice(
                    ["B_upstream_fail", "A_quota_gate", "C_timeout_poll"])
            elif status == "timeout":
                err_type = "C_timeout_poll"

            job_id = rid("job")
            up_job = rid("hf", 12)
            result_url = None
            if status == "completed":
                ext = "png" if media == "image" else "mp4"
                result_url = f"https://cdn.higgsfield.ai/mock/{job_id}.{ext}"
            body = json.dumps({"model": alias, "prompt": "hello world"})

            cx.execute(
                """INSERT INTO jobs (
                    id, api_key_id, cpa_partner_id, group_id, account_id,
                    model_alias, jst, endpoint, request_body_json, request_ts,
                    upstream_job_id, upstream_cost, result_url,
                    status, error_type, error_detail, finished_at,
                    latency_ms, poll_count,
                    actual_credits_h, charged_credits_h, refunded
                ) VALUES (?,?,?,?,?, ?,?,?,?,?, ?,?,?, ?,?,?,?, ?,?, ?,?,?)""",
                (
                    job_id, key_id, None, group_id, acct,
                    alias, jst, f"/jobs/v2/{jst}", body, ts_s,
                    up_job, actual, result_url,
                    status, err_type, None, finished,
                    latency, polls,
                    actual, charged, refunded,
                ),
            )
            n_jobs += 1

            cx.execute(
                """INSERT INTO usage_events (
                    id, ts, api_key_id, cpa_partner_id, cpa_user_id,
                    group_id, account_id, model_alias, jst, media_type,
                    upstream_cost, actual_credits_h, charged_credits_h,
                    markup_pct, status, latency_ms, poll_count, error_type,
                    higgsgo_job_id, upstream_job_id, result_url,
                    billing_month, billing_day
                ) VALUES (?,?,?,?,?, ?,?,?,?,?, ?,?,?, ?,?,?,?,?, ?,?,?, ?,?)""",
                (
                    rid("ue"), ts_s, key_id, None, None,
                    group_id or "", acct, alias, jst, media,
                    actual, actual, charged,
                    markup, status, latency, polls, err_type,
                    job_id, up_job, result_url,
                    ts.strftime("%Y-%m"), ts.strftime("%Y-%m-%d"),
                ),
            )
            n_events += 1

    cx.commit()
    cx.close()
    print(f"seeded {n_jobs} jobs and {n_events} usage_events across "
          f"{args.days} days into {args.db}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
