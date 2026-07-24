# new-api 侧接口 2 (§6.4 official-api) 接入反馈

**Context**: higgsgo 团队交付 `GET /api/pricing` (§6.2 billing feed) 和 `GET /api/pricing/official-api` (§6.4 market data feed) 两个接口给 new-api 消费, 附样本响应
`/tmp/higgsgo-wire-api-pricing.json` 和 `/tmp/higgsgo-wire-official-api.json`.

new-api 侧核对结果 + 格式建议. 写给 higgsgo 团队.

---

## TL;DR

- **接口 1 `/api/pricing`** — ✅ **new-api 侧零改动**. `controller/ratio_sync.go`
  现成的 PricingItem 解析器 (line 383-418) 已经在读 `billing_mode / billing_expr / quota_type`,
  higgs 侧配好 Upstream URL 就能吃. 前端 `parseTiersFromExpr` DSL 解析器早
  就支持样本里的 `has(param(...),...) ? tier(...) : ... : tier("unpriced",0,...)` 语法.
- **接口 2 `/api/pricing/official-api`** — ❌ **new-api 侧需要新建 model-price-adapter**.
  之前不存在 (契约 §6.4 那段"new-api uses it as the source for its
  model-price-adapter" 是**描述性期望**, 不是引用已有代码). 本次一起写.

Shape 层完全对齐, 无 breaking. 但需要 higgs 那边微调 4 个点让上线更顺.

---

## 请 higgs 侧调整的 4 项

### 1. 必修 — `observed_at` 不要是 Go 零值

样本里所有 references 的 `observed_at` 都是 `"0001-01-01T00:00:00Z"` (Go
`time.Time{}` 序列化默认值). Seed / scrape 时请写真实时间戳:

- 官方公告页 scrape → 用抓取时刻 `time.Now()`
- 无来源的手工录入 → 用录入时刻
- Import 批处理 → 用 batch 提交时刻

new-api 前端会用它做"参考价新旧提示" (>30 天飘灰 / tooltip 展示上次采集时间).
零值传到前端会显示 "1 年 0001", UI 上很怪.

### 2. 澄清 — auth token 期望

契约里写 "Bearer sk-hg-<key>, 跟 /v1/* 相同". 但 §6.4 描述是 operator-facing
informational 数据, 建议澄清:

- **是不是应该用运营 token (独立轮换)** 而不是用户 sk-hg?
- 用户 key 泄露频率高, 拿它拉 pricing feed 会导致 official-api 数据也被泄露
- new-api 侧 sync worker 用**独立配置的 upstream key**, 不复用 user token

先按 "跟 /v1/* 相同" 实现, 但请确认这是有意为之还是先行占位.

### 3. 确认 — Cache-Control header 真的加了

契约声明 `Cache-Control: public, max-age=21600` (6h). 请确认响应头**真的**
带这个:

```bash
curl -I https://<higgs>/api/pricing/official-api
# 期望看到:
# Cache-Control: public, max-age=21600
```

new-api sync worker 会读这个 header 调度轮询频率 (>= max-age 才重拉), 缺了
就退化成默认 6h 轮询 (可以工作但没走契约).

### 4. Edge case — 空 references 的模型行为

样本里已经做到"零 references 的模型 omit" ✓. 但**从 N 条变成 0 条**的场景
契约没写明:

- **推荐 (跟 §6.4 语义一致)**: 直接从响应 `data[]` 里 omit 掉整个 model entry
- **不推荐**: 保留 model entry 但 `references: []`

new-api 侧 sync 会**每次全量替换 in-memory map**, omit 语义下上次有本次无
的 model 自动被清掉, 不需要 delete-tombstone. 这也是契约 §6.4 "never emit
derived fields, low frequency" 的自然延伸.

---

## new-api 侧要做的 5 件事 (我们这边, 明天联调)

参考实现路径, 已存在的基础设施都能复用:

### 1. 后端类型定义

新建 `model/market_reference.go`, shape 直接对齐样本响应:

```go
type MarketReferenceEntry struct {
    Provider        string  `json:"provider"`
    Resolution      string  `json:"resolution"`
    Audio           string  `json:"audio"`
    Mode            string  `json:"mode"`
    Unit            string  `json:"unit"`              // "per_second" | "per_request"
    DurationSeconds float64 `json:"duration_seconds"`  // 0 = 按秒计, 非 0 = 该 duration 的整档价
    AmountMicros    int64   `json:"amount_micros"`     // USD × 1e6, 原始值不做换算
    Currency        string  `json:"currency"`          // "USD"
    SourceURL       string  `json:"source_url"`
    ObservedAt      time.Time `json:"observed_at"`
}
```

**契约红线**: 后端**不生成任何 derived 字段** (discount / savings / discount_percent /
list_price / strikethrough_price). 全部前端 `computeDiscountRatio` 现算. 契约 §6.4
Line 562-574 已经明确禁止.

### 2. 持久化容器

**不动 abilities 表** (那是 group×model×channel 三键的路由表, 跟 pricing
无关). 走 `options` 表 JSON 持久化, 跟现有 `billing_setting.BillingExpr`
同一套:

```go
// setting/market_ref_setting/market_ref.go
type MarketReferenceSetting struct {
    // key = model_name, value = 该 model 所有 references
    References map[string][]model.MarketReferenceEntry `json:"references"`
}

var marketRefSetting = MarketReferenceSetting{
    References: make(map[string][]model.MarketReferenceEntry),
}

func init() {
    config.GlobalConfig.Register("market_ref_setting", &marketRefSetting)
}
```

好处:
- 复用现成 `config.GlobalConfig.Register` 的 hot-reload / 落库机制
- 不需要 DB migration (options 表已有)
- 跟 billing_setting 对称, 未来读代码的人一眼看懂

### 3. Sync worker

新建 `controller/market_ref_sync.go`, 仿 `controller/ratio_sync.go` 结构:

```go
func SyncMarketReferences(ctx context.Context) error {
    // 1. 从 upstream 配置里拿 /api/pricing/official-api URL + auth
    // 2. GET, 尊重 Cache-Control (>= max-age 才重拉)
    // 3. 解析 {success, generated_at, data: [{model_name, references}]}
    // 4. 全量替换 in-memory map (omit 的 model 自动清掉, 见上面 Edge case 4)
    // 5. 持久化到 options 表 (config.GlobalConfig 自动 handle)
}
```

**独立错误处理**: 契约 §6.4 硬要求 "5xx 不影响 billing". Sync 挂了只是前端
不显示折扣角标, 不影响 `/api/pricing` billing feed. 具体做法:

- 独立 ticker, 独立 goroutine
- 独立 error metric (不共享 ratio_sync 的告警)
- Sync 失败保留上次 in-memory 数据 (grace period), 不清空

### 4. 前端注入

`model/pricing.go` 的 `Pricing` struct **加一个字段**:

```go
type Pricing struct {
    // ... 现有字段 ...
    MarketReference []model.MarketReferenceEntry `json:"market_reference,omitempty"`
}
```

`GetPricingModels()` 组装时按 model_name 从 `marketRefSetting.References`
里 pluck 出来塞到每个 model 上.

**注意**: 前端已经在 `types.ts` 里定义好 `MarketReferenceEntry` 类型和字段
用途, 后端 shape 保持一致即可, 前端 0 改动.

### 5. Feature flag

考虑到 higgsgo 那边 `/api/pricing/official-api` 是新端点, 加个开关兜底:

```go
// setting/system_setting/system.go
type SystemSetting struct {
    EnableMarketRefSync bool `json:"enable_market_ref_sync"`  // default false
}
```

- 关闭 → sync worker 不启动 → market_reference 字段为空 → 前端折扣角标不显示 (natural degradation)
- 打开 → 走完整流程
- 上线策略: 先默认关闭跑一周, 稳了改默认 true

---

## 时间表 (proposed)

| Day | higgsgo 侧 | new-api 侧 |
|---|---|---|
| 今天 | 修 observed_at + 确认 auth / cache header | 写后端类型 + 持久化容器 |
| 明天 | `/api/pricing/official-api` 上生产 | 写 sync worker + 前端注入 + 联调 |
| 后天 | — | 灰度打开 feature flag, 观察一天 |
| +3 | — | 默认开启, 走契约完整流程 |

---

## 附录 A — new-api 现有基础设施摘录

供 higgs 侧了解 new-api 侧的实现现状, 判断契约是不是需要微调:

**接口 1 `/api/pricing` 现成消费者位置**:
- `controller/ratio_sync.go:383-418` — PricingItem 解析器
- `setting/billing_setting/tiered_billing.go` — BillingSetting 持久化
- `pkg/billingexpr/` — DSL 解析器 (支持 tier() + has(param()))

**Pricing struct** (前端 `/api/pricing` 消费的):
- `model/pricing.go:18-56` — Pricing struct 定义
- **没有 market_reference 字段** — 契约 §6.4 强制要求跟 billing feed 分开, 是正确设计

**Abilities 表** (跟 pricing 无关的路由表):
- `model/ability.go:16-24` — group × model × channel_id 三键
- 只管"这个 group 的这个模型走哪个 channel", 不塞 pricing 数据

---

## 附录 B — 契约 §6.4 严格约束回顾

来自 `PRICING-DOWNSTREAM-CONTRACT.md`:

- ✅ new-api 前端 UI 已经就位 (2026-07-23 merge, PR #11)
- ✅ 前端 `computeDiscountRatio` 前端现算, 不发布 derived 字段
- ✅ 前端 UI 已用 `market_reference` 字段占位 (等后端注入)
- ⏳ 后端 sync + adapter 待建 (本文档描述的 5 件事)

---

## 联系人

- new-api 侧: dan (本文档作者)
- higgsgo 侧: TBD

问题 / 反馈直接改动这份文档 + comment.
