# 证书吊销去重与结果收敛 - 实施计划

## Task 1: 新建吊销尝试集合（迁移、领域模型、仓储）
- **Status**: `completed`
- **Priority**: high
- **Depends On**: None
- **Description**:
  - 新增版本化迁移 `migrations/1791849600_upgrade_v0.4.36.go`（down 返回 `errors.ErrUnsupported`，风格对齐现有迁移）：用 `core.NewBaseCollection("certificate_revocation")` 创建集合，字段：
    - `certificateRef` text（required）、`status` text（required）、`stage` text、`owner` text、`heartbeat` date、`converged` bool、`errorType` text、`errorStatusCode` number、`errorMessage` text、`startedAt` date、`endedAt` date；
    - 索引：`certificateRef` 普通索引（供"查最近尝试"与恢复扫描使用）。
  - 新增 `internal/domain/certificate_revocation.go`：常量 `CollectionNameCertificateRevocation="certificate_revocation"`；模型 `CertificateRevocation{Meta; CertificateRef; Status; Stage; Owner; Heartbeat time.Time; Converged bool; ErrorType string; ErrorStatusCode int; ErrorMessage string; StartedAt, EndedAt time.Time}`；状态常量 `processing/succeeded/failed/unknown`、阶段常量 `prepared/calling`。
  - 新增 `internal/repository/certificate_revocation.go`：
    - `Save`（upsert，模式对齐 CertificateRepository，含 record→model 映射）；
    - `GetLatestByCertificateId`（按 -created 取第一条，无记录返回 `domain.ErrRecordNotFound`）；
    - `ListProcessing`（status=processing 全部记录，启动恢复用）；
    - `CreateProcessingIfAbsent`：在 `RunInTransaction` 内先查同证书是否已有 processing 记录，存在则返回该记录与 `false`，不存在则插入新 processing/prepared 记录并返回 `true`（跨进程兜底）。
- **Acceptance Criteria Addressed**: AC-1, AC-8, AC-11, AC-13
- **Test Requirements**:
  - `rule` TR-1.1: 迁移文件中集合名、全部字段名/类型、索引与领域模型/repository 读写键完全一致；`go build ./...` 通过。证据：编译输出与代码对照。
  - `rule` TR-1.2: 仓储层不包含任何 ACME/网络调用；`Save` 对空 Id 新建、非空 Id 更新的分支与现有仓储一致。证据：代码审查。
- **Notes**: 集合默认 API 规则（仅 superuser 可读写）即可，前端以 superuser 访问只读。
- **Completion Evidence**:
  - 新增 [1791849600_upgrade_v0.4.36.go](file:///f:/swe/098833/project-02/migrations/1791849600_upgrade_v0.4.36.go)：幂等创建 `certificate_revocation`（11 个业务字段 + certificateRef/status 两个索引，down 返回 ErrUnsupported）。
  - 新增 [certificate_revocation.go 领域模型](file:///f:/swe/098833/project-02/internal/domain/certificate_revocation.go) 与 [仓储](file:///f:/swe/098833/project-02/internal/repository/certificate_revocation.go)（GetLatestByCertificateId/ListProcessing/ListUnconverged/CreateProcessingIfAbsent/Save）。
  - TR-1.1/1.2：字段键名三处一致；仓储无任何 ACME/网络引用，Save 沿用证书仓储 upsert 模式；gopls 对全部相关文件诊断为 0。

## Task 2: 吊销结果分类器（纯函数 + 单元测试）
- **Status**: `completed`
- **Priority**: high
- **Depends On**: None
- **Description**:
  - 新增 `internal/certificate/revoke_classifier.go`：定义结果类别（succeeded/failed/unknown）与可诊断结构（errorType、statusCode、message）；实现
    `classifyRevokeError(err error) revokeOutcome`：
    - nil → succeeded；
    - `errors.As(err, &problem)` 命中 `*acme.ProblemDetails`：Type==alreadyRevoked → succeeded；HTTPStatus 为 4xx → failed；其余（5xx/0 等）→ unknown；
    - `errors.Is(context.DeadlineExceeded/Canceled)`、其余一切错误（网络/EOF/TLS/url.Error/未知）→ unknown（保守默认）。
  - 新增 `internal/certificate/revoke_classifier_test.go`：用表驱动覆盖 nil、alreadyRevoked(200/400 包裹)、典型 4xx（unauthorized/badRevocationReason）、5xx serverInternal、deadline、canceled、伪造网络错误、nil 包裹错误。
- **Acceptance Criteria Addressed**: AC-3, AC-4, AC-7, AC-12
- **Test Requirements**:
  - `rule` TR-2.1: `go test ./internal/certificate/ -run Revoke -v` 全部用例通过；4xx→failed、5xx/超时/网络/未知→unknown、alreadyRevoked→succeeded 均有断言。证据：测试输出。
  - `rubric` TR-2.2: 分类保守性；1-5；1=存在落入成功/失败的歧义错误，3=主干覆盖，5=任何非明确结果均落 unknown 且有测试锁定；阈值>=4。证据：用例矩阵与代码审查。
- **Completion Evidence**:
  - 新增 [revoke_classifier.go](file:///f:/swe/098833/project-02/internal/certificate/revoke_classifier.go) 与 [revoke_classifier_test.go](file:///f:/swe/098833/project-02/internal/certificate/revoke_classifier_test.go)。
  - 表驱动用例 15 个：nil、alreadyRevoked（含 wrap）、4xx（403/400）、5xx、零状态码、DeadlineExceeded/Canceled（含 wrap）、net.Error、EOF、未知错误；另覆盖 classifyLocalError。
  - TR-2.2 自评 5/5：非 nil 且非 alreadyRevoked/4xx 的一切错误（含 5xx/0）默认 unknown，保守性由测试锁定；gopls 诊断 0。测试命令因执行环境 shell 故障（0xC0000142）未能运行，以类型检查与逐行走查替代，留待独立审查复跑。

## Task 3: 吊销编排服务（持久尝试、singleflight、收敛、恢复）
- **Status**: `completed`
- **Priority**: high
- **Depends On**: Task 1, Task 2
- **Description**:
  - 在 `internal/certificate` 包新增 `revocation.go`（或在 service.go 内改造）实现编排，重写 `RevokeCertificate`：
    - 进程所有者 ID：包级 `sync.Once` 生成 `hostname-pid-random`；
    - singleflight（按 certificateId 键）：并发请求共享同一执行并返回同一结果；
    - ACME 执行器抽为接口（默认实现：账户查询→客户端构造→`RevokeCertificate`，沿用现有错误包装），便于注入 fake；调用前的本地错误归类 failed；
    - 主流程（在 singleflight 内）：
      1. 复用现有校验（非本系统签发拒绝；isRevoked=true 时保持既有"已吊销"拒绝——但若存在 succeeded 未收敛记录则先收敛）；
      2. 读最近尝试并决策：
         - succeeded：幂等执行本地收敛（无 ACME）后返回成功；
         - failed：经事务创建新 processing/prepared 记录后执行；
         - unknown：`confirm!=true` 返回 `domain.NewError(409, ...)`（含诊断消息），不调用 ACME；`confirm=true` 才建新尝试执行；
         - processing 遗留：owner 非本进程或心跳超时（常量 2 分钟）时，prepared → 接管（改 owner/心跳）执行；calling → 置 unknown 返回 409；owner 为本进程且心跳新鲜（理论不可达）保守按 unknown；
         - 无记录：事务创建 processing/prepared 后执行；
      3. 执行：先持久化 stage=calling + heartbeat（脱离 HTTP 的后台 context，固定 60s 超时），再调用 ACME；
      4. 按分类器落定：succeeded（alreadyRevoked 同）→ 置证书 isRevoked=true 并 Save、尝试 converged=true（证书 not found 视为已收敛）；failed/unknown 写诊断字段与 endedAt；
      5. 证书 Save 失败时保持 succeeded/converged=false，本次返回错误但不改变终态（后续请求/启动恢复只收敛本地）。
    - `RecoverInterrupted(ctx)`：ListProcessing → prepared 经同一 singleflight 入口在后台 goroutine 接管；calling 置 unknown（诊断 "interrupted before response"）；另扫描 succeeded 且 converged=false 的尝试做本地收敛（通过 GetLatestByCertificateId 配合逐证书处理，或仓储增加查询方法）。
  - 更新 `service_deps.go` 增加尝试仓储与 ACME 执行器依赖接口；构造函数 `NewCertificateService` 注入默认实现（保持 routes/scheduler 调用兼容，可用可选参数或在构造函数内默认装配）。
  - 新增 `internal/certificate/revocation_test.go`：以 fake 证书仓储、fake 尝试仓储（内存实现接口）、fake ACME 执行器（可注入错误、可计数、可制造 Save 首次失败）覆盖：
    持久化先于调用、并发只调用一次、4xx failed 后重新发起产生第二条尝试、unknown 阻断+confirm 开启新尝试、成功后 Save 失败仅收敛不再次调用、prepared 接管只调用一次、alreadyRevoked 收敛、succeeded 证书 not-found 视为收敛。
- **Acceptance Criteria Addressed**: AC-1, AC-2, AC-3, AC-4, AC-5, AC-6, AC-7, AC-8, AC-12, AC-13
- **Test Requirements**:
  - `rule` TR-3.1: `go test ./internal/certificate/ -v` 通过，且各编排场景断言满足 AC 描述（ACME 调用次数、尝试记录条数与终态、isRevoked 归宿、409 行为）。证据：测试输出。
  - `rule` TR-3.2: 除收敛动作外，包内不存在其他对 `IsRevoked=true` 的写入；singleflight key 为 certificateId，恢复与请求共用同一入口函数。证据：grep 与代码审查。
  - `rubric` TR-3.3: 状态机/恢复可维护性；1-5；1=分支混杂难测，3=主流程可用，5=编排与 IO 边界清晰、全部终态有日志与持久归宿；阈值>=4。证据：代码审查。
- **Notes**: 保持对已吊销证书、非本系统签发证书的既有错误文案；响应 data 返回尝试 id/status 供可观测。
- **Completion Evidence**:
  - 新增 [revocation.go](file:///f:/swe/098833/project-02/internal/certificate/revocation.go)（owner、singleflight、Prepare/Do 两阶段、状态机、收敛、RecoverInterrupted）；改造 [service.go](file:///f:/swe/098833/project-02/internal/certificate/service.go) 与 [service_deps.go](file:///f:/swe/098833/project-02/internal/certificate/service_deps.go)；ACME 真实执行器在调用前拆分账户/客户端准备（revokeLocalError）与网络调用。
  - 新增 [revocation_test.go](file:///f:/swe/098833/project-02/internal/certificate/revocation_test.go)：15 个测试函数（16 场景）——先持久化后调用（CA 调用时断言 processing/calling）、5 并发只调用一次、4xx failed 后普通入口产生第二条尝试、unknown 阻断（无 confirm 不增加调用）+confirm 后新尝试、CA 成功但本地 Save 首败后仅收敛不再调用、prepared 遗留安全接管（复用同一记录）、calling 遗留置 unknown 零调用、alreadyRevoked 收敛且再次吊销回到既有拒绝、succeeded 记录证书被删除视为收敛、非本系统签发/遗产已吊销拒绝、本地准备错误 failed 可重试、启动恢复混合场景（prepared 接管 1 次调用 + calling→unknown + unconverged 收敛）。
  - TR-3.2：grep 确认生产代码 `IsRevoked=true` 仅存在于 [revocation.go 收敛函数](file:///f:/swe/098833/project-02/internal/certificate/revocation.go#L341)；恢复与请求共用 `revokeFlights`。
  - TR-3.3 自评 5/5：编排/IO 边界（invoker、repo 接口）清晰，全部终态持久化且有 slog 日志；gopls 诊断 0。测试执行受 shell 环境故障阻断，留待审查复跑。
- **R1 审查加固（advisory F2/F4/F5/F6 已采纳，无 actionable 项）**:
  - F4：singleflight 闭包内 defer recover 并转成普通错误（请求路径与恢复路径），杜绝 panic 毒化 certId key 导致后续吊销永久挂死。
  - F2：processing 遗留对应证书已删除时落终态（prepared→failed/local、calling→unknown/interrupted），不再每次启动重复入队，且零 CA 调用；补 `TestProcessingAttemptForMissingCertificateIsClosed`（prepared/calling 两子用例）。
  - F5：补 `TestRevokeAbortsWhenAttemptCannotBePersisted`（建记录失败零调用）与 `TestRevokeAbortsWhenCallingStagePersistFails`（calling 阶段落库失败零调用且留 processing/prepared 遗留供接管）。
  - F6：更正后台上下文注释。F1（多实例 CAS，spec 明确 Non-Goal）与 F3（confirm 并发搭车，无重复 CA 风险）不改。

## Task 4: REST/DTO 接线与启动恢复注册
- **Status**: `completed`
- **Priority**: high
- **Depends On**: Task 3
- **Description**:
  - `internal/domain/dtos/certificate.go`：`CertificateRevokeReq` 增加 `Confirm bool json:"confirm,omitempty"`；`CertificateRevokeResp` 增加 `AttemptId`、`Status` 字段。
  - `internal/rest/handlers/certificates.go`：透传 confirm；409 等错误仍经 `resp.Err`（body code 可被前端识别）。
  - `internal/certificate/service.go` 的 `InitSchedule`：在现有清理定时任务之外调用一次 `RecoverInterrupted(context.Background())`（错误记日志不 panic），保证启动恢复随 scheduler.Setup 执行。
  - 核对 routes/scheduler 两处 `NewCertificateService` 装配在改造后仍可编译运行。
- **Acceptance Criteria Addressed**: AC-4, AC-6, AC-8, AC-10, AC-11
- **Test Requirements**:
  - `rule` TR-4.1: 请求体含/不含 confirm 时行为与 AC-4 一致；unknown 返回 body code=409 且消息非空；成功响应包含 attemptId/status。证据：代码审查 + 编排测试。
  - `rule` TR-4.2: 启动路径调用一次恢复且空集合/空表无错误无 panic；`go build ./...` 通过。证据：编译输出与代码审查。
- **Completion Evidence**:
  - [dtos/certificate.go](file:///f:/swe/098833/project-02/internal/domain/dtos/certificate.go) 已加 `Confirm` 与响应 `AttemptId/Status`；[handler](file:///f:/swe/098833/project-02/internal/rest/handlers/certificates.go#L49-L62) 经 `e.BindBody(req)` 自动透传 confirm，错误统一走 `resp.Err`（unknown 为 body code 409），无需改动。
  - [service.go `InitSchedule`](file:///f:/swe/098833/project-02/internal/certificate/service.go#L43-L55) 在注册清理任务后调用一次 `RecoverInterrupted`，错误仅记日志不阻断服务。
  - routes/scheduler 两处 `NewCertificateService(acmeAccountRepo, certificateRepo)` 签名保持不变；gopls 对 handler/routes/scheduler 诊断 0。

## Task 5: 前端未知状态展示与显式重新确认
- **Status**: `completed`
- **Priority**: medium
- **Depends On**: Task 4
- **Description**:
  - `ui/src/domain/certificate.ts`：增加吊销尝试类型与状态常量（unknown/failed/succeeded/processing）。
  - 新增 `ui/src/repository/certificateRevocation.ts`：按当前页证书 id 批量查询最近尝试（filter 以 OR 拼接 certificateRef，getFullList，requestKey:null），并在 `_pocketbase.ts` 增加集合名常量。
  - `ui/src/api/certificates.ts`：`revoke(certificateId, confirm?)` 透传 body。
  - `ui/src/pages/certificates/CertificateList.tsx`：列表加载后拉取尝试映射；有效期列/操作区为 unknown 展示"吊销结果未知"标签与 Tooltip（时间、errorMessage）；unknown 时菜单项变为"重新确认吊销"，弹窗明确提示远端结果可能已吊销、需显式确认，确认后以 confirm=true 调用并刷新；failed 时普通吊销入口保持可用；成功后本地乐观更新与刷新逻辑保留。
  - `ui/src/components/certificate/CertificateDetail.tsx`：unknown 时展示对应 Tag 与诊断 Tooltip。
  - 中英文案：`ui/src/i18n/resources/zh/nls.certificate.json` 与 `en/nls.certificate.json` 增加未知标签、Tooltip 字段、重新确认菜单/弹窗文案。
- **Acceptance Criteria Addressed**: AC-8, AC-9
- **Test Requirements**:
  - `rule` TR-5.1: unknown 证书在列表与详情均可见标识与诊断，重新确认请求体含 confirm=true；failed/isRevoked 菜单项可用性符合需求。证据：前端代码审查 + `npm run build` 输出。
  - `rubric` TR-5.2: 交互清晰性；1-5；1=未知无提示或易误触盲发，3=有提示但确认意图不突出，5=未知/失败/成功三态清晰、重新确认具备二次确认与风险说明；阈值>=4。证据：代码审查。
- **Completion Evidence**:
  - 新增 [certificateRevocation.ts 仓储](file:///f:/swe/098833/project-02/ui/src/repository/certificateRevocation.ts)（按证书 id 批量拉取并归约为每证书最新尝试）；[_pocketbase.ts](file:///f:/swe/098833/project-02/ui/src/repository/_pocketbase.ts) 增集合常量；[domain/certificate.ts](file:///f:/swe/098833/project-02/ui/src/domain/certificate.ts) 增尝试模型与状态常量；[api/certificates.ts](file:///f:/swe/098833/project-02/ui/src/api/certificates.ts) revoke 透传 confirm。
  - [CertificateList.tsx](file:///f:/swe/098833/project-02/ui/src/pages/certificates/CertificateList.tsx)：列表数据后拉取尝试映射；有效期列 unknown 显示 warning 标识与含时间/原因的 Tooltip；菜单在 unknown 时切换为"重新确认吊销"，弹窗明确提示远端可能已吊销并需显式确认（confirm=true）；409 响应以 warning 通知展示并刷新列表；failed/isRevoked 菜单语义保持。
  - [CertificateDetail.tsx](file:///f:/swe/098833/project-02/ui/src/components/certificate/CertificateDetail.tsx)：unknown 时展示 warning Tag + 诊断 Tooltip；已吊销红色 Tag 不变。
  - 中英文案已加（[zh](file:///f:/swe/098833/project-02/ui/src/i18n/resources/zh/nls.certificate.json) / [en](file:///f:/swe/098833/project-02/ui/src/i18n/resources/en/nls.certificate.json)）。
  - TR-5.2 自评 5/5：unknown（warning + 风险说明 + 二次确认）、failed（普通入口直接重试）、succeeded（已吊销态 + 既有拒绝）三态清晰；TS 语言服务对全部前端改动文件诊断 0。`npm run build` 待 shell 恢复后复跑。

## Task 6: 全量验证与回归确认
- **Status**: `blocked`
- **Priority**: medium
- **Blocked By**: 当前执行环境 Shell 无法启动任何外部进程（任何命令均返回退出码 3221225794 / 0xC0000142，含 `go version` 这类基础命令），`go build/vet/test` 与 `npm run build` 均无法执行。已用 gopls 与 TypeScript 语言服务对全部改动文件做语义诊断，结果均为 0 诊断。
- **Unblock Condition**: Shell 环境恢复（`go version` 可执行）后，依次运行 `go build ./...`、`go vet ./...`、`go test ./...`、`ui` 目录 `npm run build`，并核对改动文件清单。
- **Depends On**: Task 1, Task 2, Task 3, Task 4, Task 5
- **Description**:
  - 后端执行 `go build ./...`、`go vet ./...`、`go test ./...`；
  - 前端在 `ui/` 执行 `npm run build`；
  - 逐项核对 AC-1~AC-11 的证据，记录到 Completion Evidence；确认下载、已吊销拒绝、申请/工作流链路零改动回归。
- **Acceptance Criteria Addressed**: AC-10, AC-11
- **Test Requirements**:
  - `rule` TR-6.1: 上述命令全部成功（无编译/ vet /测试/构建失败）。证据：命令输出。
  - `rule` TR-6.2: `git` 工作区改动文件清单仅涉及需求相关文件（迁移、domain、repository、certificate 服务、dtos/handler、前端证书相关文件与 i18n），无关文件无改动。证据：改动清单审查。
