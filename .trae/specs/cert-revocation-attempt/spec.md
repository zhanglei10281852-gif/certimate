# 证书吊销去重与结果收敛 - 产品需求文档

## Overview
- **Summary**: 为管理员手动吊销证书引入"先持久化吊销尝试、再调用 ACME"的确定性流程：持久化吊销尝试记录及其所有者，同一证书的并发请求复用/观察同一尝试；按 CA 响应把尝试落定为成功、确定失败或结果未知三类终态，并提供崩溃接管、本地收敛、显式重新确认与客户端可诊断能力。
- **Purpose**: 消除"超时/页面报错后因本地仍显示未吊销而向 CA 重复发送吊销请求"的风险；保证 CA 已成功但本地保存失败时不再重复调用 CA 即可完成本地状态收敛；保证远端结果未知时不盲重发。
- **Target Users**: 系统管理员（发起吊销、观察吊销状态、在未知状态下显式重新确认的人）。

## Goals
- 调用 ACME 吊销接口之前，持久化吊销尝试及其当前所有者；同一证书的并发吊销请求复用或观察同一尝试。
- 区分三类结果：CA 明确成功（含 `alreadyRevoked`）、CA 明确拒绝（确定失败，可按现有入口重新发起）、远端结果未知（超时/连接中断/5xx/进程在调用途中退出）。
- CA 已明确成功但本地 `isRevoked` 保存失败时，恢复流程只做本地收敛，不再调用 CA。
- 所有者在真正发起 ACME 调用之前退出（崩溃/重启）时，重启后的实例可以安全接管该尝试。
- 未知状态对客户端可见、可诊断（时间、原因），禁止自动盲重发；只有管理员显式重新确认才能开启新的尝试。
- 吊销状态转换、证书列表、统计、工作流"是否跳过旧证书"的判断来自同一份持久结果（证书记录的 `isRevoked`，仅由成功尝试的收敛动作置位）。

## Non-Goals
- 不改造工作流自动申请/续期链路，不新增自动吊销能力（仅管理员手动吊销入口）。
- 不引入多实例集群分布式锁；PocketBase 为内嵌 SQLite 单实例部署，跨进程安全由"持久化检查 + 事务"兜底，并发协调以进程内 singleflight 为主。
- 不向 CA 主动查询吊销结果（ACME 无标准吊销查询接口）；未知状态只能等待显式重新确认。
- 不改变证书下载、删除、上传、申请、部署等既有功能语义。

## Background & Context
- 现状入口：`POST /api/certificates/{id}/revoke` → [certificates.go](file:///f:/swe/098833/project-02/internal/rest/handlers/certificates.go) → [service.go `RevokeCertificate`](file:///f:/swe/098833/project-02/internal/certificate/service.go#L219-L257)。
- 现状流程：读证书 → 校验（非本系统签发拒绝、已吊销拒绝）→ 构造 ACME 客户端 → 调用 [`ACMEClient.RevokeCertificate`](file:///f:/swe/098833/project-02/internal/certacme/client_revoke.go#L14-L25) → 成功后才 `certificate.IsRevoked=true` 并 Save。
- 现状缺陷：
  1. ACME 调用前无任何持久化，超时/断连/页面报错后本地仍为未吊销，管理员重复点击会重复向 CA 发送吊销请求；
  2. 并发点击无协调，多个请求可同时通过 `IsRevoked` 检查并重复调用 CA；
  3. CA 成功但本地 Save 失败时，再次操作会再次调用 CA（只能依赖 CA 返回 alreadyRevoked 兜底）；
  4. 进程在调用过程中退出后无恢复依据；
  5. 无"未知"状态，客户端无法区分"未吊销"与"吊销结果未知"。
- 依赖的错误形态（lego v5.3.1）：CA 明确应答为 `*acme.ProblemDetails`（含 HTTPStatus、Type、Detail），`alreadyRevoked` 为 `urn:ietf:params:acme:error:alreadyRevoked`；超时（`context.DeadlineExceeded`）、连接中断（网络错误/EOF/TLS 错误）、进程退出不会携带 ProblemDetails，属结果未知。
- 持久化使用 PocketBase 迁移建表（参考 v0.4.3 增加字段、v0.4.15 修改集合的写法）；仓储模式参考 [certificate.go](file:///f:/swe/098833/project-02/internal/repository/certificate.go)。
- `isRevoked` 的现有消费方：[统计 SQL](file:///f:/swe/098833/project-02/internal/repository/statistics.go#L59)、[工作流跳过旧证书判断](file:///f:/swe/098833/project-02/internal/workflow/engine/executor_bizapply.go#L214)、前端证书列表/详情。本次保证这些消费方读到的仍然是唯一权威结果，不要求它们改判。

## Functional Requirements

- **FR-1（尝试先持久化）**：在发起 ACME 吊销调用之前，必须先在新集合 `certificate_revocation` 中持久化一条尝试记录，包含证书引用、状态 `processing`、阶段 `prepared`、当前所有者标识（进程级唯一）、心跳时间、开始时间；没有持久化尝试不得调用 ACME。

- **FR-2（并发复用/观察）**：同一证书的并发吊销请求必须复用同一个尝试：进程内通过 singleflight 让并发调用方共享同一执行过程并得到一致结果；尝试记录是唯一事实来源。不得因并发产生两次 ACME 吊销调用。

- **FR-3（阶段协议与接管）**：真正调用 ACME 前将同一记录更新为阶段 `calling` 并刷新心跳。启动（服务启动恢复）或按需处理时发现 `processing` 遗留记录：
  - 阶段为 `prepared`（所有者在真正调用前退出）→ 重启后的实例可安全接管：改写所有者后执行一次吊销流程；
  - 阶段为 `calling`（调用途中退出，远端结果未知）→ 不得自动重发，置为 `unknown` 并保留诊断信息。

- **FR-4（结果分类与落定）**：ACME 调用结果分类如下，终态必须持久化（结束时间、诊断字段）：
  - 成功（nil 错误）→ `succeeded`；
  - CA 返回 `alreadyRevoked` → 视为远端已吊销，按成功处理并本地收敛；
  - CA 明确拒绝（携带 `*acme.ProblemDetails` 且 HTTP 4xx）→ `failed`，记录错误类型/状态码/消息，证书保持未吊销，允许经现有吊销入口重新发起（产生新尝试记录）；
  - 超时、`context` 取消、连接中断、TLS/EOF 等网络错误、HTTP 5xx ProblemDetails、以及其他无法确认远端结果的错误 → `unknown`，记录错误类型/消息；
  - ACME 调用前的本地错误（账户不存在、客户端初始化失败等，确定未触达 CA）→ `failed`，允许重新发起。
  - 分类保守原则：任何不能确定为"成功"或"明确 4xx 拒绝"的错误一律归入 `unknown`。

- **FR-5（本地收敛不重复调用 CA）**：尝试落定 `succeeded` 后执行本地收敛：置证书 `isRevoked=true` 并保存，成功后标记尝试 `converged=true`。若证书保存失败，尝试保持 `succeeded/converged=false`；后续请求命中该尝试时、以及启动恢复时，只重试本地保存，不再调用 ACME。证书记录已被删除（not found）视为已收敛。

- **FR-6（未知状态阻断与显式重新确认）**：当证书最近一次尝试为 `unknown` 时：
  - 普通吊销请求不得调用 CA，返回可识别的错误（HTTP 业务码 409 及诊断消息）；
  - 仅当请求显式携带重新确认标记（`confirm=true`）时，才允许开启一条新的尝试并调用 ACME；
  - `failed` 终态不阻断，普通请求即可开启新尝试；`succeeded` 终态只做幂等收敛/返回。

- **FR-7（执行上下文隔离）**：吊销的 ACME 调用使用脱离 HTTP 请求生命周期的后台上下文（带有限超时），避免管理员关闭页面/请求取消直接掐断在途调用；调用方页面提前离开后，终态仍被持久化，可被后续查询观察到。

- **FR-8（启动恢复）**：服务启动时扫描吊销尝试集合并一次性恢复：`prepared` 遗留经同一 singleflight 入口接管执行；`calling` 遗留置 `unknown`；`succeeded` 且未收敛的仅做本地收敛。恢复过程不得产生重复 ACME 调用。

- **FR-9（客户端可见与可诊断）**：前端证书列表与详情必须能展示最近一次尝试的未知状态（独立标识/标签与提示，包含时间与原因），并提供"显式重新确认吊销"的操作，确认后以 `confirm=true` 调用；明确失败后现有"吊销证书"入口仍可直接重新发起。中英文案齐备。

- **FR-10（兼容性）**：
  - 未吊销证书的下载行为保持不变；
  - 已吊销证书再次吊销仍返回既有的拒绝错误（"could not revoke a certificate which is already revoked"）；
  - 非本系统签发证书的拒绝语义不变；
  - 普通证书申请、续期、无需吊销的工作流行为不变；
  - 证书统计、工作流跳过旧证书继续读取证书记录的 `isRevoked`；该字段只能由成功尝试的收敛动作置为 true，unknown/failed 期间保持 false。

## Non-Functional Requirements

- **NFR-1（可构建性）**：后端 `go build ./...`、`go vet ./...`、`go test ./...` 通过；前端 `npm run build`（tsc + vite）通过。
- **NFR-2（可测试性）**：错误分类与尝试状态决策为纯函数/可注入依赖的编排逻辑，具备不依赖数据库的单元测试覆盖（分类表、状态机决策、fake ACME/fake 仓储）。
- **NFR-3（可诊断性）**：每次尝试持久化所有者、阶段、心跳、起止时间、错误类型/状态码/消息与收敛标志，服务端日志记录关键迁移点。
- **NFR-4（迁移安全）**：新集合通过版本化迁移创建，字段/索引齐全；已存在环境升级与全新初始化都能正常启动；迁移具备与现有迁移一致的幂等风格。
- **NFR-5（最小影响面）**：复用现有 repository/service/handler/migration 分层与命名风格，不改动无关链路。

## Constraints
- **Technical**: Go 1.25；PocketBase v0.39.10（core.TextField/BoolField/NumberField/DateField、RunInTransaction）；lego v5.3.1（acme.ProblemDetails）；React + Ant Design 前端；PocketBase superuser 鉴权下前端可直接读取新集合。
- **Business**: 不改变现有 REST 路径与请求/响应基本形态，仅在请求体增加可选 `confirm` 字段、响应 data 增加尝试状态信息；错误仍经 `resp.Err` 返回。
- **Dependencies**: 依赖迁移机制创建集合；依赖进程启动钩子（scheduler.Setup）执行恢复。

## Assumptions
- 单实例部署为主；并发协调以进程内 singleflight 为准，持久化事务检查用于跨进程/重启兜底。
- ACME 吊销为单次快速 HTTP 调用，后台上下文超时取固定常量（60 秒）；超时即按 unknown 处理，符合"不盲重发"原则。
- `alreadyRevoked` 表示远端确定已吊销，本地收敛为 isRevoked=true 是安全且幂等的。
- 前端以 superuser 身份访问 PocketBase，新集合默认（仅 superuser）API 规则即可被前端只读访问。

## Acceptance Criteria

### AC-1: ACME 调用前持久化尝试与所有者
- **Type**: `rule`
- **Given**: 一张本系统签发、未吊销的证书
- **When**: 管理员发起吊销
- **Then**: 在任何 ACME 吊销网络调用发生之前，`certificate_revocation` 中已存在该证书的尝试记录（status=processing、stage=prepared、owner 非空、heartbeat/startedAt 已写）；若记录创建失败则不会调用 ACME
- **Pass Condition**: 代码路径证明"先建记录后调用"顺序成立，且不存在绕过该记录直接调用 ACME 的吊销路径；单元测试（fake 调用器）断言调用 ACME 前已有持久化记录
- **Evidence**: 代码审查、单元测试输出

### AC-2: 并发请求复用同一尝试且只调用一次 CA
- **Type**: `rule`
- **Given**: 同一证书同时到达 N 个吊销请求
- **When**: 吊销流程执行
- **Then**: N 个请求共享同一尝试记录与同一次执行，ACME 调用恰好发生一次，所有调用方得到一致终态
- **Pass Condition**: 并发单元测试中 fake ACME 调用计数为 1，且尝试记录只有一条 processing 记录
- **Evidence**: 单元测试输出、代码审查

### AC-3: CA 明确拒绝记录确定失败且可重新发起
- **Type**: `rule`
- **Given**: ACME 返回携带 ProblemDetails 的 4xx 拒绝（如 unauthorized）
- **When**: 吊销流程落定
- **Then**: 尝试为 failed（含错误类型/状态码/消息），证书 isRevoked 保持 false；随后普通吊销请求可创建一条新尝试并再次调用 ACME
- **Pass Condition**: 分类单元测试覆盖 4xx → failed；编排测试断言 failed 后新请求产生第二条尝试记录并再次调用；接口返回错误（保持既有错误返回形态）
- **Evidence**: 单元测试输出、代码审查

### AC-4: 结果未知时持久化未知、阻断盲重发、显式确认后重试
- **Type**: `rule`
- **Given**: 吊销调用发生超时/连接中断/5xx，或遗留 calling 记录
- **When**: 流程落定或启动恢复执行
- **Then**: 最近尝试为 unknown（含诊断信息）且 isRevoked 保持 false；此后不带 confirm 的吊销请求不调用 ACME 并返回业务码 409；携带 confirm=true 时才创建新尝试并调用一次 ACME
- **Pass Condition**: 分类单元测试覆盖 DeadlineExceeded/网络错误/5xx/未知错误 → unknown；编排测试断言 unknown 后普通请求 ACME 调用数不增加、confirm=true 后增加一次；遗留 calling 记录被恢复为 unknown
- **Evidence**: 单元测试输出、代码审查

### AC-5: CA 成功后本地保存失败仅收敛本地、不再调用 CA
- **Type**: `rule`
- **Given**: ACME 返回成功，但证书 isRevoked 保存首次失败（尝试 succeeded/converged=false）
- **When**: 再次对该证书发起吊销请求，或服务启动恢复执行
- **Then**: 不发生任何 ACME 调用，仅重试本地保存使 isRevoked=true、尝试 converged=true；证书记录已删除时直接视为已收敛
- **Pass Condition**: 编排测试模拟 Save 首次失败、后续成功，断言 ACME 总调用次数为 1、最终 isRevoked=true；not-found 分支断言收敛完成且无 ACME 调用
- **Evidence**: 单元测试输出、代码审查

### AC-6: prepared 阶段退出可被重启实例安全接管
- **Type**: `rule`
- **Given**: 一条 processing/prepared 遗留尝试（所有者已不存在），且从未进入 calling
- **When**: 服务启动恢复
- **Then**: 新实例接管所有者并只执行一次吊销流程；同一证书不会因接管产生一次以上 ACME 调用
- **Pass Condition**: 编排/恢复测试断言接管后 ACME 恰好调用一次并正常落定；恢复路径与请求路径共用同一 singleflight 入口
- **Evidence**: 单元测试输出、代码审查

### AC-7: alreadyRevoked 按成功收敛且不再调用 CA
- **Type**: `rule`
- **Given**: ACME 返回 ProblemDetails 类型 alreadyRevoked
- **When**: 吊销流程处理响应
- **Then**: 尝试落定 succeeded，本地 isRevoked 收敛为 true，之后不再调用 ACME
- **Pass Condition**: 分类单元测试覆盖 alreadyRevoked → 成功；编排测试断言本地收敛且无第二次 ACME 调用
- **Evidence**: 单元测试输出、代码审查

### AC-8: 吊销状态、列表、统计、工作流跳过来自同一持久结果
- **Type**: `rule`
- **Given**: 各类尝试终态
- **When**: 检查证书列表、统计 SQL、工作流 checkCanSkip 与吊销状态转换
- **Then**: isRevoked 仅由 succeeded 尝试的收敛动作置位；unknown/failed 期间为 false；统计与工作流跳过逻辑无需改判即与吊销状态一致；列表展示的已吊销/未知状态可回溯到尝试记录
- **Pass Condition**: 全局搜索确认除收敛逻辑外无其他位置写 isRevoked；统计/工作流仍只读该字段；前端未知标识来自尝试记录
- **Evidence**: 代码审查、grep 证据

### AC-9: 未知状态客户端可见、可诊断并可显式重新确认
- **Type**: `rule`
- **Given**: 证书最近尝试为 unknown
- **When**: 管理员查看证书列表/详情并操作
- **Then**: 列表与详情展示"吊销结果未知"标识及时间/原因提示；存在显式"重新确认吊销"操作，确认后带 confirm=true 请求；明确失败时普通"吊销证书"入口仍可用
- **Pass Condition**: 前端代码实现上述展示与交互，且 `npm run build` 通过；中英文案齐备
- **Evidence**: 前端代码审查、构建输出

### AC-10: 既有功能兼容
- **Type**: `rule`
- **Given**: 未吊销证书下载、已吊销证书再次吊销、非本系统签发证书吊销、普通证书申请与无需吊销的工作流
- **When**: 分别操作
- **Then**: 下载行为不变；已吊销/非本系统签发仍返回既有拒绝文案；申请与工作流链路无回归
- **Pass Condition**: 既有校验与错误文案保持；改造范围外代码无行为变更；全量 go build/test 与前端构建通过
- **Evidence**: 代码审查、构建与测试输出

### AC-11: 迁移可升级、服务可启动并完成恢复
- **Type**: `rule`
- **Given**: 已存在部署的数据库与全新数据库
- **When**: 执行新版本迁移并启动服务
- **Then**: `certificate_revocation` 集合及全部字段、索引被创建；服务正常启动并执行一次启动恢复扫描，无 panic
- **Pass Condition**: 迁移文件符合现有迁移规范（版本号递增、down 返回 ErrUnsupported）；编译通过；启动恢复对空集合无副作用
- **Evidence**: 迁移代码审查、go build 输出

### AC-12: 状态机与错误分类的保守完备性
- **Type**: `rubric`
- **Dimension**: 状态机覆盖度与"宁可未知、不可盲发"的保守性
- **Scale**: 1-5
- **Anchors**: 1 = 存在未分类错误可导致再次盲发或状态悬空；3 = 主要分支覆盖但有个别边界依赖默认行为；5 = 全部错误形态与遗留/并发/删除边界均有确定归宿，默认归宿为 unknown，无任何盲重发路径
- **Pass Threshold**: >= 4
- **Evidence**: 单元测试用例矩阵、代码审查

### AC-13: 分层一致性与诊断信息质量
- **Type**: `rubric`
- **Dimension**: 与现有 repository/service/handler/前端分层风格的一致性及日志/诊断字段的可用性
- **Scale**: 1-5
- **Anchors**: 1 = 分层混乱、无诊断信息；3 = 基本可用但风格不一或诊断不足；5 = 严格沿用现有模式，尝试记录字段足以支撑排障（所有者/阶段/心跳/错误/时间线）
- **Pass Threshold**: >= 4
- **Evidence**: 代码审查

## Open Questions
- 无（关键决策已按需求与代码现状确定：新集合承载尝试、isRevoked 为唯一收敛结果、alreadyRevoked 视为成功、unknown 需 confirm=true）。
