# Model × Tier 关系文档

> Higgsfield 账号会员档、每个 model 的可用性、免费额度、unlim bundle 三层关系的说明。也解释了 higgsgo 的路由如何自动挑账号。
>
> **最后校对**: 2026-07-20 (v0.5.6)
> **数据来源**: `higgsfield-register/docs/PRICING-OFFICIAL.md` + agent 三 tier 实测（`/tmp/tier_probe_results.json`）+ `higgsfield-register/server/data/` 系列 audit
> **代码引用**: `data/reference/verified-models.json` + `data/reference/model-specs-extra.json`

---

## 1. Higgsfield 的会员档

只有 5 个真实付费档（`/subscriptions/v2/plans` 返回）+ 1 个 free 隐含档：

| plan_type | 月价 | 月 credits | 并发 (V/I) | 免费额度 |
|---|---|---|---|---|
| **free** | $0 | 10 daily | 1/1 | daily_credits=10 |
| **starter** | $19 | 270 | 2/4 | 见 §4 |
| **plus** | $49 | 1,200 | 6/8 | 见 §4 |
| **pro** | $?? | 620 (实测) | ? | 见 §4 |
| **ultra** | $99 | 3,000 | 8/8 | 见 §4 + 6 model unlim |
| **scale** | $139 | 2,500 | ? | ? |
| **team** | $59 | 1,000 | ? | ? |

**代码里的 tier_rank**（`internal/domain/account.go`）：
```
free=0  starter=1  basic=2  pro=3  plus=4  ultra/ultimate/scale/creator/team/enterprise=5
```

**注意**：higgsfield 官方**没有 basic 档**。我们代码保留 `PlanBasic` 常量是历史遗留（防御性排序），实际生产账号不会返回 `plan_type: "basic"`。

---

## 2. 每个 model 需要什么 tier

**核心事实**（agent 2026-07-20 三 tier 实测得出）：

**Higgsfield 上游 128 个 live model 里，0 个返回 `402 payment_required` 或 `403 tier_gate`。**  
只要账号是 **starter 及以上**，认证通过（返回 422 body_error = "有权限，只是 body 没填全"）。

**唯一被硬 gate 的是 `plan_type=free`。**（实测 2026-07-20 07:53:56：free 号请求 seedream-v4-5 → 403 gate。）

### 简化后的 min_plan 规则（v0.5.3 起）

`data/reference/model-specs-extra.json` 里所有 129 个 alias 都标 `min_plan: "starter"`，含义：
- **starter / plus / pro / ultra** 账号都能用
- **free** 账号被 higgsgo 的 pool 过滤挡下（不会打到上游）

### Dead endpoints

2 个 upstream 404 的 endpoint 被标 `endpoint_status: "dead"`，registry 应该 skip 它们：
- `cinematic-studio-2-5`
- `flux-kontext`

### 会不会有更严的 gate？

`sealed.json` 的 `d_class_by_gate` 记录了从早期 free 号 403 反弹推测的 11 组"gated model"（veo3_family_pro / sora2_creator_pro / kling_advanced_pro 等），字面意思是"这些要 Pro"。**但 agent 用 starter 号真调时都过了 tier gate**。可能有两种解释：
1. Higgsfield 后端把 gate 从 model level 移到了 job level（认证通过但真跑时可能 fail）
2. 这些 gate 只对 free 生效

**保守做法**：`verified-models.json` 里 `starter_locked=true` / `requires_paid=true` 的字段保留，`deriveMinPlan` 会算出 `pro`（更严）。extras 里我们标了 `starter`，loader 取 MAX（更严胜出），最终生效的还是 `pro`。**这样两个数据源冲突时选严的**，是防御性设计。

### 变化：如果哪天 402 出现了怎么办？

`error_type=gate` 会记录到 `usage_events`，`fail_streak` 累积。运维可以：
- WebUI 上 pause 该账号
- 或未来的 auto-failover 会自动 pause（TODO）

---

## 3. Unlim Bundle（免 credits）

**是什么**：Plus / Pro / Ultra 账号可以在 higgsfield 上花钱**加购**某个 model 的 unlim bundle（如"nano_banana_2 4K unlim 7 天 = $35"）。持有 bundle 的账号跑该 model 时**不扣月度 credits**，走 `_unlimited` 后缀的 job_set_type。

**bundle 目录**（从实测的 `/subscriptions/bundle/unlim-catalog` 抓的完整表 —— 见 `higgsfield-register/pricing/higgs-pricing-verified.md`）：

| bundle_type | 对应 model | 分辨率 | 时长 | 价格（1d/7d/14d, credits×100）|
|---|---|---|---|---|
| nano_banana_2_2k | nano_banana_pro_unlimited | 1k, 2k | — | 500 / 2500 / 5000 |
| nano_banana_2_4k | nano_banana_pro_unlimited | 1k, 2k, 4k | — | 700 / 3500 / 7000 |
| seedance_2_720p | seedance_2_unlimited | 480p, 720p | 15s | 7300 / 29400 / 58800 |
| seedance_2_1080p | seedance_2_unlimited | 480p, 720p, 1080p | 15s | 14900 / 63000 / 127000 |
| seedance_fast_and_mini | seedance_unlimited + seedance_mini_unlimited | 默认 | 默认 | 3500 / 21000 / 42000 |
| gpt_image_2_1k | gpt_image_2_unlimited | 1k | — | 1500 / 7500 / 15000 |
| gpt_image_2_2k | gpt_image_2_unlimited | 1k, 2k | — | 2000 / 10000 / 20000 |
| gpt_image_2_4k | gpt_image_2_unlimited | 1k, 2k, 4k | — | 3000 / 15000 / 30000 |
| kling_3_1080p | kling_3_unlimited | 1080p | — | 3500 / 17500 / 35000 |
| kling_3_4k | kling_3_unlimited | 1080p, 4k | — | 6000 / 36000 / 72000 |
| all_above | 六合一 | max | max | 17900 / 78000 / 157000 |

**Starter 账号 `/subscriptions/bundle/unlim-catalog` 返回 `{"bundles":[]}`** —— starter 完全不能买 unlim。

### model-specs-extra.json 里的字段

对 6 个支持 unlim 的 model：
```json
{
  "nano-banana-2": {
    "unlim_job_set_type": "nano_banana_pro_unlimited",
    "unlim_bundle_types": ["nano_banana_2_2k", "nano_banana_2_4k"]
  }
}
```

- `unlim_job_set_type` — 该 model 对应的 unlim endpoint
- `unlim_bundle_types` — 哪些 bundle 可以解锁

支持 unlim 的 6 个 model：`nano-banana-2` / `gpt-image` / `kling-3` / `seedance-1.5` / `seedance-2-0` / `seedance-2-0-mini`

### higgsgo 怎么用（TODO）

**v0.5.6 有 `prefer_unlim` 开关但还是 dormant**。要生效需要：
1. 建 `account_unlim_activations` 表
2. Refresher 定期 GET `/workspaces/unlim-activations` 同步到本地
3. Pick 时 join 该表 `WHERE job_set_type = model.unlim_job_set_type AND (expires_at IS NULL OR expires_at > now())`

**当前状态**：team 后台在做（agent id `ae2885e8350503c29`）。

---

## 4. Free Quota（免费额度）

**是什么**：每个 tier 每月自带一些 model 的免费次数，跑这些 model 不扣通用 credits。字段在 `/user` API 返回。

### 实测三 tier 的 `/user` 免费额度字段

| 字段名 | starter | plus | pro | 用途 |
|---|---|---|---|---|
| `face_swap_credits` | 2 | 2 | 2 | face-swap / face-swap-v2 |
| `qwen_camera_control_credits` | 0.4 | 0 | 0 | qwen-camera-control |
| `soul_credits` | 0 | 0 | 0 | soul-*, text2image-soul* |
| `character_swap_credits` | 0 | 0 | 0 | character-swap* |
| `text2keyframes_credits` | 0 | 0 | 0 | text2keyframes |
| `wan2_5_video_credits` | 0 | 0 | 0 | wan2-5-* |
| `veo3_fast_generations_count` | 0 | 0 | 0 | veo3-fast |
| `daily_credits` | 0 | 0 | 0 | free 号才有 (10/day) |

**注意**：0 的字段不意味着"永不发放"，可能是当月已用完 or 该 tier 该月不发。有些字段是浮点（`qwen_camera_control_credits=0.4`）—— 表示"0.4 次" or "40% 概率"，具体 Higgsfield 语义未细究。

### model-specs-extra.json 里的字段

```json
{
  "face-swap": { "free_quota_field": "face_swap_credits" },
  "qwen-camera-control": { "free_quota_field": "qwen_camera_control_credits" },
  ...
}
```

### higgsgo 怎么用（TODO）

**v0.5.6 有 `prefer_free_quota` 开关但还是 dormant**。要生效需要：
1. `accounts` 表加 7 列存这些 quota 字段
2. Refresher 从 `FetchUser` 已返回的字段抽出来存到本地
3. Pick 时对应 model 的 `free_quota_field` 列 > 0 的账号优先

**当前状态**：team 后台在做（agent id `ae2885e8350503c29`）。

---

## 5. higgsgo 的路由逻辑（v0.5.6 现状）

### Group 层 `route_strategy`（用户可选，2 选 1）

- **`load_balance`**（默认）— 系统自动挑最合适的账号
- **`priority`** — 按管理员设的 `accounts.priority DESC` 硬排序

### `load_balance` 内部 6 个 knob（Settings 面板可调）

| Setting Key | 默认 | 生效状态 |
|---|---|---|
| `tier_aware` | ON | ✅ 生效 —— 便宜档优先烧 |
| `prefer_unlim` | OFF | 🕐 dormant, team 在做 |
| `prefer_free_quota` | OFF | 🕐 dormant, team 在做 |
| `prefer_richer` | OFF | ✅ 生效 —— 高余额优先 |
| `balance_headroom_pct` | 120 | ✅ 生效 —— 余额 ≥ cost × 120% 才能被 pick |
| `jitter` | ON | ✅ 生效 —— 同分随机抖动 |

### 完整 SQL 排序（当 `load_balance` + defaults）

```sql
SELECT ... FROM accounts
WHERE status IN ('active','throttled-recovered')
  AND in_flight_jobs < 5              -- 并发上限
  AND (max_concurrent = 0 OR in_flight_jobs < max_concurrent)  -- F4
  AND subscription_balance >= 120% × cost  -- headroom
  AND tier_rank(plan_type) >= model.min_plan_rank
ORDER BY
  tier_rank ASC,                      -- 便宜档优先（cheap-first）
  last_used_at ASC,                   -- LRU 均匀轮转
  in_flight_jobs ASC,                 -- 负载最少
  RANDOM()                            -- 抖动
LIMIT 1
```

### 举例：seedream-v4-5（min_plan=starter, cost=100）

**场景**：qiaozhi group 里有 starter + pro + plus 各 1 号，全 idle。

1. **WHERE 过滤**：3 个都符合（都 ≥ starter tier, balance 都够）
2. **ORDER BY**：tier ASC → starter(rank=1) 先; pro(3) 次; plus(4) 最后
3. **pick starter** —— 消耗 starter 的 100 credits

**结果**：Plus/Pro 一分不烧，只烧 Starter。以后 seedream-v4-5 请求都优先 starter，除非 starter 4 个 concurrent 满了或 balance 不够（升到 pro，再升到 plus）。

---

## 6. 每次踩坑的核对流程

生产遇到 `upstream 402/403 gate` 时：

1. 打日志看 picked_account 的 `plan`（`journalctl -u higgsgo | grep picked`）
2. **如果是 free 号 → v0.5.3 起 gate 已经挡下，不该出现**。若出现说明数据脏，改 `verified-models.json` 或 `model-specs-extra.json` 补一下
3. **如果是 starter+ 号 → 说明该 model 隐藏 gate**，需要在 `model-specs-extra.json` 里手动升 `min_plan`（如 seedream-v4-5 需要显式 `min_plan: "basic"` 之类）
4. Refresh: `scp model-specs-extra.json vps22:/opt/higgsgo/data/reference/ && ssh vps22 systemctl restart higgsgo`
5. 记录到本文档 §2 的"conflict list"

---

## 附录：数据文件位置

- **上游 pricing scrape**: `higgsfield-register/docs/PRICING-OFFICIAL.md`
- **完整 sealed 分类**: `higgsfield-register/server/data/sealed.json` (216 model 的 A/B/C/D/X)
- **每个 model 的 verified metadata**: `data/reference/verified-models.json` (129 alias)
- **higgsgo 补充字段**: `data/reference/model-specs-extra.json` (min_plan / dead / unlim / free_quota)
- **实测 tier probe 报告**: `/tmp/tier_probe_results.json` (2026-07-20，本地临时，未落库)
- **实测三 tier 的 `/user` /  unlim-catalog / plans / costs**: `/tmp/hf_probe_local/`（本地临时）
