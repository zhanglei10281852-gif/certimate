# 证书吊销去重与结果收敛 - 独立审查

- [x] CP-1: ACME 调用前已持久化尝试与所有者
  - **Type**: `rule`
  - **Covers**: AC-1 / TR-1.2 / TR-3.1
  - **Evidence**: R1 逐行确认唯一 CA 调用点 revocation.go:75 之前存在事务建 processing/prepared 与 calling 落库；calling Save 失败即 return 不调用；TestRevokePersistsAttemptBeforeCACallAndConverges 在 CA 调用内断言记录状态；R1/R2 加固补两条持久化故障用例（createErr、saveFailures）断言 calls=0。**pass**

- [x] CP-2: 同一证书并发请求只产生一次 ACME 调用且结果一致
  - **Type**: `rule`
  - **Covers**: AC-2 / TR-3.1
  - **Evidence**: R1 确认请求入口与启动恢复共用 revokeFlights（key=certificateId），事务 check-then-insert 兜底；TestRevokeConcurrentRequestsShareSingleCACall 在调用闸门处等待 5 个请求，断言 calls=1、尝试记录=1、错误全 nil。**pass**

- [x] CP-3: CA 4xx 明确拒绝落定 failed 并可经现有入口重新发起
  - **Type**: `rule`
  - **Covers**: AC-3 / TR-2.1 / TR-3.1
  - **Evidence**: 分类器 4xx→failed（含 errors.As 穿透 lego 包装）；TestRevokeExplicitRejectionFailsAndAllowsNewAttempt 断言 failed/403、isRevoked 保持 false、普通请求产生第二条尝试且 calls=2。**pass**

- [x] CP-4: 远端结果未知时持久化 unknown、阻断盲重发、confirm=true 才开启新尝试
  - **Type**: `rule`
  - **Covers**: AC-4 / TR-2.1 / TR-3.1 / TR-4.1
  - **Evidence**: 5xx/Deadline/Canceled/net.Error/兜底均 unknown；TestRevokeUnknownBlocksBlindResendUntilConfirmed 断言 1→(无 confirm 不增调用不建记录)→confirm 后 2；遗留 calling 经 TestCallingLeftoverBecomesUnknownWithoutCallingCA 与恢复测试断言零调用置 unknown；409 经 resp.Err→_api ClientResponseError.status 链路打通。**pass**

- [x] CP-5: CA 成功后本地保存失败只做本地收敛、不再调用 CA（含证书已删除）
  - **Type**: `rule`
  - **Covers**: AC-5 / TR-3.1
  - **Evidence**: TestRevokeSucceededButLocalSaveFailsConvergesWithoutSecondCall（首败后第二次请求 calls 仍 1、isRevoked=true、converged=true）；TestSucceededAttemptWithDeletedCertificateIsConverged（证书删除→零调用标记 converged）。**pass**

- [x] CP-6: prepared 遗留可被重启实例安全接管（仅一次调用）；calling 遗留置 unknown
  - **Type**: `rule`
  - **Covers**: AC-6 / TR-3.1
  - **Evidence**: TestPreparedLeftoverIsSafelyTakenOver（复用同一记录、owner 改写、calls=1）；启动恢复混合测试断言 prepared 接管 + calling→unknown + unconverged 收敛总 calls=1；R2 新增"证书已删除的 processing 遗留落终态不重复入队"用例。**pass**

- [x] CP-7: alreadyRevoked 按成功收敛且后续保持既有"已吊销"拒绝语义
  - **Type**: `rule`
  - **Covers**: AC-7 / AC-10 / TR-2.1
  - **Evidence**: 分类器优先判 alreadyRevoked→succeeded；TestAlreadyRevokedResponseConvergesLocally 断言收敛、calls=1、第二次请求返回逐字一致的 "could not revoke a certificate which is already revoked"。**pass**

- [x] CP-8: isRevoked 仅由成功尝试收敛置位，列表/统计/工作流跳过读取同一持久结果
  - **Type**: `rule`
  - **Covers**: AC-8 / TR-3.2
  - **Evidence**: grep 确认生产写入口仅 revocation.go 收敛函数；statistics.go、executor_bizapply.go 只读未改；前端未知标识来自 certificate_revocation 记录；unknown/failed 期间 isRevoked=false。**pass**

- [x] CP-9: 未知状态在列表/详情可见、可诊断，并提供显式重新确认交互
  - **Type**: `rule`
  - **Covers**: AC-9 / TR-5.1
  - **Evidence**: R1 静态走查：列表有效期列 warning 标识+Tooltip(时间/errorMessage)、菜单切换"重新确认吊销"+风险弹窗+confirm=true、409 warning 通知并刷新；详情 warning Tag；中英文案齐备；TS strict 无违规。npm build 未运行（见 CP-12）。**pass（静态）**

- [x] CP-10: 下载、非本系统签发拒绝、申请/工作流链路兼容性
  - **Type**: `rule`
  - **Covers**: AC-10
  - **Evidence**: 下载链路零改动；两条拒绝文案逐字保持并有测试（TestRevokeCompatibilityGuards）；非本系统签发在任何尝试逻辑前拦截（零尝试零调用）；构造函数签名不变，routes/scheduler 装配不受影响。**pass**

- [x] CP-11: 迁移可升级、集合字段/索引正确、启动恢复可执行
  - **Type**: `rule`
  - **Covers**: AC-11 / TR-1.1 / TR-4.2
  - **Evidence**: R1 核对版本号 1791849600 最高、11 字段三处一一对应、索引 DDL 合 PB 既有风格、幂等守卫、down=ErrUnsupported；恢复随 scheduler.Setup 在 e.Next() 前执行一次，空集合无副作用无 goroutine。**pass**

- [ ] CP-12: 全量构建与测试通过（go build/vet/test、npm run build）
  - **Type**: `rule`
  - **Covers**: AC-10 / AC-11 / NFR-1 / TR-6.1
  - **Evidence**: **blocked** — R1 与 R2 两次独立审查、主会话多次尝试，任何子进程（含 `go version`、`echo hello`、`where.exe go`）均返回 3221225794（0xC0000142 STATUS_DLL_INIT_FAILED），命令无法执行。替代证据：gopls 与 TypeScript 语言服务对全部 21 个改动/新增文件语义诊断均为 0；两轮全新上下文逐行走查（含 lego v5.3.1、PocketBase v0.39.10、x/sync v0.22.0 实际源码核对）未发现编译/类型/测试断言问题。需在可启动进程的环境复跑：`go build ./...`、`go vet ./...`、`go test ./...`、`ui` 下 `npm run build`。

- [x] CP-U1: 状态机覆盖度与"宁可未知、不可盲发"的保守性
  - **Type**: `rubric`
  - **Covers**: AC-12 / TR-2.2 / TR-3.3
  - **Scale**: 1-5
  - **Anchors**: 1 = 存在错误/边界导致盲重发或状态悬空；3 = 主分支覆盖；5 = 全部错误形态与遗留/并发/删除边界均有确定归宿，默认 unknown
  - **Pass Threshold**: >= 4
  - **Evidence**: R1 评分 **5/5**：全部入口分支有确定归宿，默认 unknown，无盲重发路径；R2 加固删除证书遗留、panic 边界后覆盖更完整。仅多实例 CAS（F1）在 spec 明确 Non-Goal 之外。**pass**

- [x] CP-U2: 分层一致性与诊断信息质量
  - **Type**: `rubric`
  - **Covers**: AC-13
  - **Scale**: 1-5
  - **Anchors**: 1 = 分层混乱、无诊断；3 = 基本可用；5 = 沿用现有模式且尝试记录足以排障
  - **Pass Threshold**: >= 4
  - **Evidence**: R1 评分 **5/5**：沿用 repository/service/handler/migration 既有模式；owner/stage/heartbeat/起止时间/错误三元组/converged 字段齐备，关键迁移点有 slog。**pass**

## Review History

### Review R1
- **Result**: `blocked`（全部实质检查点 pass；CP-12 因环境 Shell 故障 blocked；无 actionable 发现，6 条 advisory：F1 多实例接管 CAS、F2 缺失证书 processing 遗留无终态、F3 confirm 并发搭车、F4 singleflight panic 毒化、F5 持久化故障缺测试、F6 注释误导）
- **Evidence**: 独立子代理全新上下文，通读全部产物与 lego/PB/backoff 实际依赖源码；命令 `go version`/`go build`/`echo hello` 均 0xC0000142；grep 证实唯一 CA 调用点与 isRevoked 唯一写入口。
- **Blocked By**: 执行环境无法启动任何子进程。
- **Resume When**: Shell 恢复后复跑 CP-12 命令。
- **处置**: F2/F4/F5/F6 已实现并补测试；F1（spec Non-Goal）与 F3（无重复 CA 风险）明确不改。

### Review R2
- **Result**: `blocked`（加固本身 pass，无回归、无 actionable；CP-12 仍因环境 blocked）
- **Evidence**: 独立子代理全新上下文复审 F2/F4/F5/F6；对照 x/sync@v0.22.0 源码确认 fn 内 recover 后 call 正常完成、key 释放、等待者收到同一普通 error；逐行验证 3 个新测试断言与代码路径一致；未发现导入/闭包/死代码回归。新增 3 条 nit 级 advisory（F4 注释精确性已即时采纳修正；stage 枚举未来扩展复核；close Save 持续失败属环境 best-effort），均不阻塞。
- **Blocked By**: 同 R1（0xC0000142）。
- **Resume When**: 同 R1。

### 总体结论
代码与规格层面：**通过**（CP-1~CP-11、CP-U1、CP-U2 均有独立静态证据）。唯一未闭环项为 CP-12 的命令行构建/测试执行，阻塞原因为当前环境无法启动进程，与代码无关；环境恢复后运行上述四条命令即可完成最终验收。
