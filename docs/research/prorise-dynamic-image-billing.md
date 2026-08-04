# 动态图片计费研究笔记

本文记录的是 `Prorise-cool/new-api` 这条动态图片计费线的实现脉络。重点不是“功能有没有”，而是它是怎样穿过规则层、relay 注入层、计费层、日志层和前端展示层的。

## 背景

这条线的核心目标，是把图片请求里的「数量」和「规格倍率」拆开处理，再把可配置的 SKU 规则注入到预扣费和结算链路中，避免同一维度被重复计费。

最早的主锚点是提交 [bda45921f802177a6bd7a4334d95cebb7d8cdf1f](https://github.com/Prorise-cool/new-api/commit/bda45921f802177a6bd7a4334d95cebb7d8cdf1f)。后续在合并和 UI 调整中，相关能力被保留并继续修正，见 [0350213cc989c79a717241a3d649c2eab4afa0d7](https://github.com/Prorise-cool/new-api/commit/0350213cc989c79a717241a3d649c2eab4afa0d7) 和 [bde9b2f44887d34ec54799ae191d50f97914359e](https://github.com/Soein/new-api/commit/bde9b2f44887d34ec54799ae191d50f97914359e)。

## 代码链路

### 1. 规则层先把 SKU 计算编译成热路径可读的数据

在 `setting/ratio_setting/sku_ratio.go` 里，SKU 规则配置包含 `rules`、`model_rules`、`enabled`、`max_total_ratio`，并用 `atomic.Pointer[compiledSkuRules]` 做热路径无锁读取。规则解析时把 `n`、`seconds`、`duration` 之类的字段排除在 SKU 的 `out_key` 之外，避免它们被误当成可叠加倍率。

`max_total_ratio` 不是简单拒绝，而是会在超限时做等比缩放，保证最后写回的倍率总量不会突破上限。这个行为是这条线里最需要向使用者解释清楚的边界之一。

来源：
- [setting/ratio_setting/sku_ratio.go](https://github.com/Prorise-cool/new-api/blob/bda45921f802177a6bd7a4334d95cebb7d8cdf1f/setting/ratio_setting/sku_ratio.go)
- [bda45921f802177a6bd7a4334d95cebb7d8cdf1f](https://github.com/Prorise-cool/new-api/commit/bda45921f802177a6bd7a4334d95cebb7d8cdf1f)

### 2. relay 层把图片 SKU 倍率注入计费对象

`controller/relay.go` 在进入 `ModelPriceHelper` 之前先求图片 SKU 倍率，再把结果写进 `relayInfo.PriceData.AddOtherRatio(...)`。对于按次定价的 DALL-E 场景，如果 SKU 已经覆盖了内建的 `size/quality`，会把 `meta.ImagePriceRatio` 置为 `1.0`，从而避免规格倍率和 SKU 倍率双算。

在当前仓库里，图像请求的等价注入点仍然能在 `controller/relay.go` 看到：图片请求会直接走 `GetTokenCountMeta()`，即便 `CountToken` 关闭，也会保留图片计费所需的倍率元数据。

来源：
- [controller/relay.go#L156-L170](https://github.com/Soein/new-api/blob/main/controller/relay.go#L156-L170)
- [controller/relay.go#L291-L293](https://github.com/Soein/new-api/blob/main/controller/relay.go#L291-L293)

### 3. 图片请求 DTO 明确拆分“数量”和“规格”

`relaykit/dto/openai_image.go` 把 `N` 设计成 `*uint`，并在 `GetTokenCountMeta()` 里将它放进 `BillingRatios["n"]`；而 `ImagePriceRatio` 只负责 size / quality 这类规格倍率。这样做的结果是：

- 计数 `n` 是独立维度
- 规格倍率是独立维度
- 预扣费和结算都可以分别对这两个维度做组合

对应的测试也把这个约束锁住了：`n` 缺省时默认是 `1`，负数 multipart 值会被拒绝，而且 `BillingRatios["n"]` 必须和请求里的数量一致。

来源：
- [relaykit/dto/openai_image.go#L157-L170](https://github.com/Soein/new-api/blob/main/relaykit/dto/openai_image.go#L157-L170)
- [relaykit/types/request_meta.go#L20-L31](https://github.com/Soein/new-api/blob/main/relaykit/types/request_meta.go#L20-L31)
- [relay/helper/openai_image_request_test.go#L122-L159](https://github.com/Soein/new-api/blob/main/relay/helper/openai_image_request_test.go#L122-L159)

### 4. 价格层把外部倍率统一合并

`relay/helper/price.go` 会把 `meta.BillingRatios` 逐项塞进 `PriceData.AddOtherRatio()`，再通过 `ApplyOtherRatiosToFloat(...)` 计算最终预扣费额度。也就是说，图片计费不是在某个孤立 helper 里“顺手算掉”，而是进入了统一的价格对象，和其他请求倍率一起参与计算。

来源：
- [relay/helper/price.go#L146-L195](https://github.com/Soein/new-api/blob/main/relay/helper/price.go#L146-L195)

### 5. `PriceData` 负责过滤非法倍率

`types/price_data.go` 里，`AddOtherRatio()` 会丢弃非正数、`NaN` 和正无穷倍率；`OtherRatios()` 返回的是快照副本，而不是内部 map 的直接引用；`OtherRatioMultiplier()` 和 `ApplyOtherRatiosToDecimal()` 只对有效倍率做连乘。

这意味着：动态图片计费能否稳定，关键不只在 SKU 规则本身，还在于所有后续乘法都必须走这个对象，而不是绕过它直接改 map。

来源：
- [types/price_data.go#L35-L108](https://github.com/Soein/new-api/blob/main/types/price_data.go#L35-L108)
- [service/task_billing_test.go#L148-L170](https://github.com/Soein/new-api/blob/main/service/task_billing_test.go#L148-L170)

### 6. 日志层把倍率和饱和信息一起暴露

预扣费路径和任务计费路径都会把倍率快照写进消费日志：`service/text_quota.go` 负责在写日志前调用 `attachQuotaSaturation(...)`，`service/task_billing.go` 也会做同样的附加。

`service/log_info_generate.go` 里，`attachQuotaSaturationToOther(...)` 会把饱和信息挂到 `other.admin_info.quota_saturation`，并输出一条带请求上下文的 warn 日志。这个设计的意义是，管理员能看到异常饱和，但普通用户看不到内部诊断字段。

来源：
- [service/log_info_generate.go#L21-L52](https://github.com/Soein/new-api/blob/main/service/log_info_generate.go#L21-L52)
- [service/text_quota.go#L520-L539](https://github.com/Soein/new-api/blob/main/service/text_quota.go#L520-L539)
- [service/task_billing.go#L18-L66](https://github.com/Soein/new-api/blob/main/service/task_billing.go#L18-L66)

## 关键提交时间线

| 提交 | 角色 | 结论 |
| --- | --- | --- |
| [bda45921f802177a6bd7a4334d95cebb7d8cdf1f](https://github.com/Prorise-cool/new-api/commit/bda45921f802177a6bd7a4334d95cebb7d8cdf1f) | 主引入点 | 建立动态图片 SKU 计费主链路：规则编译、relay 注入、价格对象合并、日志可见性。 |
| [0350213cc989c79a717241a3d649c2eab4afa0d7](https://github.com/Prorise-cool/new-api/commit/0350213cc989c79a717241a3d649c2eab4afa0d7) | 合并保留点 | 合并说明里明确提到保留了“SKU parameter billing”等本地特性，说明这条线在后续同步中没有被回滚掉。 |
| [bde9b2f44887d34ec54799ae191d50f97914359e](https://github.com/Soein/new-api/commit/bde9b2f44887d34ec54799ae191d50f97914359e) | UI 修正点 | 主要修正模型定价页的 unset-price 复制、加载反馈和 memo equality，不改算法本身，但说明前端展示层还在持续磨合。 |

## 前后端展示

### 后端配置入口

当前仓库里，计费设置页把模型定价相关内容挂在 `model-pricing` tab 下；默认值和类型定义都已经覆盖到 `BillingSettings`。虽然文件名已经转成“动态定价”风格，但它承载的还是同一类模型价格配置能力。

来源：
- [web/src/features/system-settings/billing/section-registry.tsx#L105-L117](https://github.com/Soein/new-api/blob/main/web/src/features/system-settings/billing/section-registry.tsx#L105-L117)
- [web/src/features/system-settings/billing/index.tsx#L27-L107](https://github.com/Soein/new-api/blob/main/web/src/features/system-settings/billing/index.tsx#L27-L107)
- [web/src/features/system-settings/types.ts#L247-L329](https://github.com/Soein/new-api/blob/main/web/src/features/system-settings/types.ts#L247-L329)

### 用户侧展示

当前仓库没有继续沿用旧的 `sku-ratio-*` 命名，而是把用户侧展示收敛成动态定价组件：

- `web/src/features/pricing/lib/dynamic-price.ts` 负责判断 `tiered_expr` 动态定价并拆出 tier / request rules
- `web/src/features/pricing/components/dynamic-pricing-breakdown.tsx` 负责把表达式拆成可读表格
- `web/src/features/pricing/components/model-details.tsx` 负责在模型详情页里挂出动态定价摘要和 breakdown

这说明 SKU 思路在当前仓库里已经和 dynamic pricing 语义合流了，展示层不再强调“SKU”字面名，而是直接展示用户能看懂的计价表达式。

来源：
- [web/src/features/pricing/lib/dynamic-price.ts#L65-L182](https://github.com/Soein/new-api/blob/main/web/src/features/pricing/lib/dynamic-price.ts#L65-L182)
- [web/src/features/pricing/components/dynamic-pricing-breakdown.tsx#L47-L260](https://github.com/Soein/new-api/blob/main/web/src/features/pricing/components/dynamic-pricing-breakdown.tsx#L47-L260)
- [web/src/features/pricing/components/model-details.tsx#L581-L582](https://github.com/Soein/new-api/blob/main/web/src/features/pricing/components/model-details.tsx#L581-L582)
- [web/src/features/pricing/components/model-details.tsx#L913-L914](https://github.com/Soein/new-api/blob/main/web/src/features/pricing/components/model-details.tsx#L913-L914)
- [web/src/features/pricing/components/model-details.tsx#L1182-L1182](https://github.com/Soein/new-api/blob/main/web/src/features/pricing/components/model-details.tsx#L1182-L1182)

## 风险和边界

1. `n`、`seconds`、`duration` 这类字段必须继续保持黑名单逻辑，否则 SKU 规则会把“数量”误当“倍率”。
2. `max_total_ratio` 的缩放策略会改变最终倍率，但不会让请求失败；这对运营配置是友好的，对排障则要求日志里必须保留原始值和缩放后值。
3. `PriceData.AddOtherRatio()` 会静默丢弃非法倍率，所以任何新乘数来源都必须先保证自己输入合法，否则配置看起来生效，实际却被过滤掉。
4. 饱和信息只写进 `other.admin_info.quota_saturation`，普通用户看不到；这符合权限隔离，但也意味着客服排查必须走管理员视图。

## 结论

这条动态图片计费线不是单点改动，而是一条闭环：

`SKU 规则编译 -> relay 注入 -> image DTO 拆分计数和规格 -> PriceData 统一合并倍率 -> 预扣费/结算 -> 日志透明化 -> 前端展示`

如果后续要继续改这块，优先检查两件事：

1. 有没有新的乘数来源绕开 `PriceData.AddOtherRatio()`
2. 有没有新的 UI / 导入路径把 `n`、`seconds`、`duration` 重新混回倍率维度

## 与当前项目的差异判断（2026-08-04）

对比基线：

- `Prorise-cool/new-api` 的 `main` 最新提交是 [`1365f101ababa51e329425e9aef2784e737f1004`](https://github.com/Prorise-cool/new-api/commit/1365f101ababa51e329425e9aef2784e737f1004)，提交时间为 2026-07-12 03:08:46 UTC。
- 当前项目基线是 [`2d66a6ed80d0b23ba1ed1678ed1c676f934fcd46`](https://github.com/Soein/new-api/commit/2d66a6ed80d0b23ba1ed1678ed1c676f934fcd46)，提交时间为 2026-08-03 23:17:21 +08:00。当前项目已经比参考 fork 更新，前端目录、计费安全边界和动态表达式能力均有后续演进，因此不适合直接 cherry-pick SKU 提交。

### 当前项目已经具备的能力

1. 固定价格图片模型已经把规格和数量拆开：DALL-E 的 `size/quality` 进入 `ImagePriceRatio`，`n` 独立进入 `BillingRatios["n"]`，并受 `MaxImageN` 上限保护。
2. `PriceData` 已经封装 `AddOtherRatio`、快照复制、倍率连乘及非法倍率过滤，SKU 无需另造乘法管线。
3. `tiered_expr` 支持 `param(path)` 和可视化 Request Rules，理论上可按 `size`、`quality`、`background`、`n` 对同步图片请求做条件倍率。
4. 动态表达式和匹配 tier 已进入模型广场与消费日志，已有“一条表达式、一份计费真相”的架构。

来源：

- [relaykit/dto/openai_image.go#L133-L170](https://github.com/Soein/new-api/blob/2d66a6ed80d0b23ba1ed1678ed1c676f934fcd46/relaykit/dto/openai_image.go#L133-L170)
- [relay/helper/price.go#L89-L195](https://github.com/Soein/new-api/blob/2d66a6ed80d0b23ba1ed1678ed1c676f934fcd46/relay/helper/price.go#L89-L195)
- [relay/helper/billing_expr_request.go#L13-L61](https://github.com/Soein/new-api/blob/2d66a6ed80d0b23ba1ed1678ed1c676f934fcd46/relay/helper/billing_expr_request.go#L13-L61)
- [web/src/features/system-settings/models/tiered-pricing-editor.tsx#L202-L251](https://github.com/Soein/new-api/blob/2d66a6ed80d0b23ba1ed1678ed1c676f934fcd46/web/src/features/system-settings/models/tiered-pricing-editor.tsx#L202-L251)

### 本轮改造前仍然缺少的能力

| 能力 | 当前状态 | 影响 |
| --- | --- | --- |
| 任意图片模型的可配置 `size/quality/background` 倍率 | 固定价格模式只有 DALL-E 硬编码；表达式模式需手写/切换整个计费模式 | 运营不能在保留现有模型价格的同时，只追加参数倍率 |
| 固定美元/张的表达式语义 | v1 表达式输出按“美元/百万 token”换算，没有直观的 `per_request()` / `per_image()` 原语 | 固定单价只能写成放大一百万倍的常量，难维护且容易配置错 |
| 表达式模式的实际图片数量结算 | `tiered_expr` 在 `ModelPriceHelper` 早分支返回，不继承固定价格路径的系统 `BillingRatios["n"]`；OpenAI 实际图片数修正又只在 `UsePrice` 时生效 | 用 `param("n")` 只能看到请求数量，不能复用固定价格路径“请求数量/实际上游数量/断流保护”的完整不变量 |
| multipart 图片编辑参数 | 生产路径的 `param(path)` 只从 `application/json` 原始 body 取值；`BuildBillingExprRequestInputFromRequest` 仅用于渠道测试 | `/v1/images/edits` 等 multipart 请求无法可靠用表达式读取 `size/quality/n`，参考 fork 的“已解析 DTO 拍平”方案更完整 |
| 参数倍率的实际命中明细 | 日志保存表达式和 matched tier，但不保存每个 Request Rule 的实际倍率快照 | 用户能看到规则，却不一定能直观看到本次请求具体命中了哪几个倍率 |
| task 型图片/视频的表达式动态计费 | task 路径仍走 `ModelPriceHelperPerCall + adaptor.EstimateBilling`，不执行 `tiered_expr` | 表达式方案目前只覆盖同步 relay，不能替代参考 fork 的跨图片/视频 SKU 层 |

来源：

- [relay/helper/price.go#L95-L98](https://github.com/Soein/new-api/blob/2d66a6ed80d0b23ba1ed1678ed1c676f934fcd46/relay/helper/price.go#L95-L98)
- [relay/helper/price.go#L296-L368](https://github.com/Soein/new-api/blob/2d66a6ed80d0b23ba1ed1678ed1c676f934fcd46/relay/helper/price.go#L296-L368)
- [relay/channel/openai/relay_image.go#L25-L30](https://github.com/Soein/new-api/blob/2d66a6ed80d0b23ba1ed1678ed1c676f934fcd46/relay/channel/openai/relay_image.go#L25-L30)
- [relay/helper/billing_expr_request.go#L13-L61](https://github.com/Soein/new-api/blob/2d66a6ed80d0b23ba1ed1678ed1c676f934fcd46/relay/helper/billing_expr_request.go#L13-L61)
- [relay/relay_task.go#L180-L252](https://github.com/Soein/new-api/blob/2d66a6ed80d0b23ba1ed1678ed1c676f934fcd46/relay/relay_task.go#L180-L252)
- [service/log_info_generate.go#L316-L331](https://github.com/Soein/new-api/blob/2d66a6ed80d0b23ba1ed1678ed1c676f934fcd46/service/log_info_generate.go#L316-L331)

## 建议

结论是“**有条件需要改**”：

- 如果只使用当前已支持的 DALL-E 档位，或图片模型完全按上游 token usage 计价，现状没有必须立即修复的计费错误。
- 如果目标是让管理员给任意生图模型配置“基础价 × 尺寸 × 质量 × 背景 × 数量”，当前项目确实存在产品能力缺口，应当补齐。

不建议直接移植参考 fork 的完整 `sku_ratio_setting`。它会在当前项目的 `billingexpr` 之外形成第二套价格真相，并且参考 fork 的代码基线落后于当前项目。更合适的实现顺序是：

1. 为 `billingexpr` 增加版本化的固定请求/固定图片价格原语（例如 `per_image(0.04)`），避免用 `40000` 之类的魔法常量表达 `$0.04/张`。
2. 增加由系统维护的图片数量变量，预扣使用已校验的请求 `n`，结算复用现有实际上游数量和断流保护逻辑；管理员规则不能覆盖该数量变量。
3. 在现有 Request Rules 编辑器中增加图片预设（`size`、`quality`、`background`），由 UI 生成同一条 Billing Expression，而不是另存一张 SKU 表。
4. 扩展表达式 trace，记录本次请求实际命中的条件倍率；日志和模型广场继续从同一表达式派生展示。
5. 如果还要覆盖 task 型图片/视频，再单独把版本化 Billing Snapshot 接入 task 的预扣、提交修正和完成结算；此时必须吸收参考 fork 的三条安全经验：禁止覆盖 `n/seconds/duration`、SKU/规则倍率持久化、`AdjustBillingOnSubmit` 不得整体清掉动态倍率。

参考 fork 最值得复用的是安全边界，而不是其存储形态：

- 图片注入与 DALL-E 内建倍率防双算：[controller/relay.go#L154-L167](https://github.com/Prorise-cool/new-api/blob/1365f101ababa51e329425e9aef2784e737f1004/controller/relay.go#L154-L167)、[controller/relay.go#L304-L338](https://github.com/Prorise-cool/new-api/blob/1365f101ababa51e329425e9aef2784e737f1004/controller/relay.go#L304-L338)
- 原子规则快照、黑名单和倍率上限：[setting/ratio_setting/sku_ratio.go#L46-L52](https://github.com/Prorise-cool/new-api/blob/1365f101ababa51e329425e9aef2784e737f1004/setting/ratio_setting/sku_ratio.go#L46-L52)、[setting/ratio_setting/sku_ratio.go#L176-L216](https://github.com/Prorise-cool/new-api/blob/1365f101ababa51e329425e9aef2784e737f1004/setting/ratio_setting/sku_ratio.go#L176-L216)、[setting/ratio_setting/sku_ratio.go#L256-L340](https://github.com/Prorise-cool/new-api/blob/1365f101ababa51e329425e9aef2784e737f1004/setting/ratio_setting/sku_ratio.go#L256-L340)
- task 提交修正时合并而非清除动态倍率：[relay/relay_task.go#L253-L271](https://github.com/Prorise-cool/new-api/blob/1365f101ababa51e329425e9aef2784e737f1004/relay/relay_task.go#L253-L271)
- 模型广场和账单倍率透明化：[model/pricing.go#L411-L421](https://github.com/Prorise-cool/new-api/blob/1365f101ababa51e329425e9aef2784e737f1004/model/pricing.go#L411-L421)、[service/billing_ratios.go](https://github.com/Prorise-cool/new-api/blob/1365f101ababa51e329425e9aef2784e737f1004/service/billing_ratios.go)

## 实施状态（2026-08-04）

本轮没有移植独立的 `sku_ratio_setting`，而是在现有“一条表达式、一份计费真相”架构中完成了同步生图链路：

- `billingexpr` v2 增加 `per_image(price)` 和系统只读 `image_count`。
- Request Rules 增加可追踪的 `rule(name, condition, multiplier)`，结算日志保存实际命中规则。
- 预扣使用经过 `MaxImageN` 校验的请求数量；结算使用有边界的上游实际数量，OpenAI 流式断连保护继续生效。
- JSON 使用归一化字段并保留原始 body 作为未知参数回退；multipart 图片编辑从解析后的 DTO 提供同一套 `param()` 视图，不保存文件内容。
- 管理后台增加按张生图预设，模型广场和消费日志按 `$ / image` 展示，不再误标成 `$ / 1M tokens`。
- 表达式配置在保存前执行编译和安全试算，负数、`NaN`、无穷及非正请求倍率会被拒绝。

仍未纳入本轮的是 task 型图片/视频计费。该路径需要可持久化的 Billing Snapshot 和任务完成结算协议，不能直接复用同步 relay 的内存请求快照，应作为独立改造处理。
