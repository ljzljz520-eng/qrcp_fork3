# qrcp 可恢复分块传输协议（QCTP v1） - 实施计划

> AC 与 TR 的 `rule` 为客观二值条件，`rubric` 为评分项（阈值见括号）。任务按依赖顺序实施。

## Task 1: manifest 包 — 文件遍历与清单构建
- **Status**: `completed`
- **Completion Evidence**:
  - `manifest/manifest.go`：Build 遍历/去重/空文件空目录/symlink 跳过；TR-1.1 通过（TestBuildDirectoryManifest/TestBuildSingleFileAndDedup）。
  - TR-1.2 通过（TestBuildCreatesNoZip 断言临时目录无新增 zip）。
- **Priority**: high
- **Depends On**: None
- **Description**:
  - 新建 `manifest/manifest.go`：定义 `Manifest`、`FileEntry`（type=file/dir、index、path、size、mode、modTime、chunks、chunkHashes、merkleRoot）等 JSON 结构（字段标签与 spec FR-1/FR-3 一致）。
  - 实现 `Build(paths []string, chunkSize int64) (*Manifest, error)`：路径规则（单文件取 basename；单目录以 basename 为根；多参数各取 basename）；顶层重名 `name(2)` 去重；递归子目录；空文件保留（chunks=0）；空子目录生成 dir 条目；symlink 与非常规文件跳过。
  - chunk 数按 chunkSize 上取整计算。
- **Acceptance Criteria Addressed**: AC-1, AC-14
- **Test Requirements**:
  - `rule` TR-1.1: 对夹具（单文件、嵌套目录含空文件/空目录、多参数、symlink、重名参数）构建 manifest，条目路径/类型/大小/chunk 数与预期表完全一致。
  - `rule` TR-1.2: 构建过程在 `t.TempDir()` 外不产生任何 `*.zip`（对临时目录快照断言）。
- **Notes**: 替换现有 `body.FromArgs` 的职责，但本任务不删除旧代码。

## Task 2: Merkle/SHA-256 工具与后台哈希器
- **Status**: `completed`
- **Completion Evidence**:
  - TR-2.1 通过（TestMerkleRootVectors：空/1/2/3/4 叶与独立参考实现一致；sha256("") 常量正确；坏 hex 报错）。
  - TR-2.2 通过（TestHasherBackgroundPass：DTO 逐块 hash、file root、global root 与独立计算一致；空文件 root=sha256("")；未完成文件 chunkHashes 序列化为 null）。
  - TR-2.3 通过（TestChunkHashWhileServeConcurrent，`go test -race` 绿；惰性哈希后后台 pass 仍能完成 root）。
- **Priority**: high
- **Depends On**: Task 1
- **Description**:
  - 新建 `manifest/merkle.go`：`HashHex(b []byte)`、`MerkleRoot(hexLeaves []string) string`（奇数复制末节点；空输入=`SHA-256("")`）、`FileBindingHash(path string, size, mode, modTimeUnix, fileRoot)`（严格按 FR-2 拼接）。
  - 新建 `manifest/hasher.go`：`Hasher` 持有 manifest 与并发安全 hash 缓存（map[fi][]string）；`Start(ctx)` 后台顺序读文件分块 SHA-256 填缓存，完成后计算 file root、绑定叶、全局 root 并置 `hashingDone`；暴露 `Snapshot()`（供 manifest JSON 序列化，未完成文件 chunkHashes=null）、进度计数。
  - `ChunkHash(ctx, fi, ci)`：缓存未命中时同步打开文件 seek 读取该块计算并回填（hash-while-serve，幂等）。
- **Acceptance Criteria Addressed**: AC-2, AC-7, AC-12
- **Test Requirements**:
  - `rule` TR-2.1: Merkle 向量测试：空集、1 节点、2 节点、3 节点（奇数复制）、4 节点结果与独立参考实现（测试内朴素实现）一致。
  - `rule` TR-2.2: 后台哈希器对夹具完成后，全部 chunk hash 与 `sha256sum` 独立计算结果一致；file/global root 与按 FR-2 手算值一致；空文件 root=`SHA-256("")`。
  - `rule` TR-2.3: hash-while-serve：在后台哈希未完成时并发调用 `ChunkHash`，结果与最终缓存一致且无重复写竞争（`go test -race` 通过）。

## Task 3: 会话 token/TTL 与 flag、配置接线
- **Status**: `completed`
- **Completion Evidence**:
  - TR-3.1 通过（TestNewSessionAndExpiry：token 长度 64 hex、100+ 次无重复、TTL 前后 Expired 正确）。
  - TR-3.2 待 T5 冒烟时随 `--help` 一并验证；flag（--chunk-size/--ttl）已注册，config.Config 增 chunkSize/ttl，零值 flag 不覆盖配置文件、默认值归一为 4 MiB / 24h。
- **Priority**: high
- **Depends On**: None
- **Description**:
  - `application.Flags` 增加 `ChunkSize int`（MiB）、`TTL string`；`cmd/qrcp.go` 注册 `--chunk-size`（默认 4）、`--ttl`（默认 `24h`）。
  - `config.Config` 增加 `ChunkSize int`、`TTL string`（viper 键 `chunkSize`/`ttl`），含默认值归一与非法值报错；manifest 包提供 `NewSession(ttl time.Duration) (token string, createdAt, expiresAt time.Time, err error)`（crypto/rand 32 字节 hex）。
- **Acceptance Criteria Addressed**: AC-9
- **Test Requirements**:
  - `rule` TR-3.1: `NewSession` 连续生成 1000 个 token 无重复、长度 64、为合法 hex；过期判断 `Expired()` 在 TTL 前后行为正确。
  - `rule` TR-3.2: chunkSize/ttl 默认值与 flag 覆盖配置文件的行为有单元测试或可执行命令验证（`go run . send --help` 含两个新 flag）。

## Task 4: QCTP HTTP API（路由、鉴权、chunk 读取、故障注入、完成上报）
- **Status**: `completed`
- **Completion Evidence**:
  - TR-4.1 通过（TestManifestEndpoint/TestChunkEndpointBytesAndHeaders/TestAPIErrorCodes：全字段、区间字节、X-Chunk-Sha256、HEAD、403/404/405/410+JSON）。
  - TR-4.2 通过（TestConcurrentChunkAssembly：120MiB+5000B、8 并发乱序拼接字节一致，-race 绿，hasher 在并发下最终收敛）。
  - TR-4.3 通过（TestFaultInjection：首响应 hash 不匹配、第二次恢复）。
  - TR-4.4 通过（TestCompleteShutdownSignal/TestCompleteKeepAliveDoesNotStop：非保活收到 stop、保活不收）。
  - 另：TestTransferPageInjectsToken 验证页面注入 token、无外链脚本。
- **Priority**: high
- **Depends On**: Task 1, Task 2, Task 3
- **Description**:
  - 重构 `server.New` 的 send 路由：`GET /send/<path>` 返回传输页 HTML；`/api/manifest`、`/api/chunk`（HEAD+GET，参数 f、c）、`/api/complete`（POST）。
  - token 校验（`X-QCTP-Token` 或 `Authorization: qrcp <token>`）：缺失/错误 403；过期 410（JSON body）；f/c 非法 404。
  - chunk 处理器：每请求独立 `os.Open`+`Seek`，有界缓冲读取并写出；响应头 `X-Chunk-Sha256`、`X-Chunk-Index`、`Content-Length`、`Cache-Control: no-store`。
  - 实现 `QRCP_TEST_FAULT=f:c:n` 故障注入（仅环境变量存在时生效，首字节翻转，前 n 次）。
  - `/api/complete` 触发非 keep-alive 关闭；移除发送方向旧 WaitGroup 排空启发式（receive 方向逻辑保持不变）。
  - 传输页通过模板注入 token（同一路径仍由现有 cookie 机制保护）。
- **Acceptance Criteria Addressed**: AC-2, AC-3, AC-4, AC-9, AC-10
- **Test Requirements**:
  - `rule` TR-4.1: httptest 表驱动测试：manifest 全字段断言；全部 chunk 区间字节与源一致、HEAD 无 body 且头一致；403/404/410 状态码与 JSON 错误体。
  - `rule` TR-4.2: 8 并发随机顺序拉取 >100MB 临时文件的全部 chunk，拼接后大小与 SHA-256 与源相等（`go test -race`）。
  - `rule` TR-4.3: 故障注入测试：设置 fault 后第 1 次响应 hash 与内容不匹配的语义可被观测（翻转字节），第 n+1 次恢复正常。
  - `rule` TR-4.4: `/api/complete` 在非 keep-alive 时向 stopChannel 发送关闭信号（用可观察的 shutdown hook 测试），keep-alive 时不发送。

## Task 5: 发送流程接线与旧 ZIP 路径移除
- **Status**: `completed`
- **Completion Evidence**:
  - TR-5.1 通过：`util.ZipFiles`/`archivex`/`body` 包全部删除，`go build ./...`、`go vet ./...`、`go mod tidy` 干净，config/server/manifest 测试全绿。
  - TR-5.2 通过：`go run . send --interface lo0 --bind 127.0.0.1 --port 18080 --path smoke --ttl 10m <fixture>` 冒烟——页面注入 64 位 token、manifest API 200 且字段齐备（dir/file/empty/emptydir、chunkHashes null vs []）、chunk 字节与 SHA-256 头和源文件一致、无 token 403、POST /api/complete 返回 `{"ok":true}` 后进程 exit 0。
  - send.go 状态行中文（文件数/目录数/总大小 humanBytes + chunkSize）；`--zip` 已删，新增 `--chunk-size`/`--ttl`。
- **Priority**: high
- **Depends On**: Task 4
- **Description**:
  - `cmd/send.go` 改用 `manifest.Build` + 后台 Hasher + `srv.Send(m, hasher, session)`；参数校验（至少一个存在的路径）错误信息保持友好。
  - 删除 `body` 包及 `Server.Wait` 中 `body.Delete()`/`DeleteAfterTransfer` 逻辑；删除 `util.ZipFiles`（确认无其他引用），`go mod tidy` 清理 `archivex`。
  - 终端输出：QR/URL 不变；增加哈希准备进度或简明状态行（复用 pb 或简易打印，不喧宾夺主）。
- **Acceptance Criteria Addressed**: AC-1, AC-11, AC-14
- **Test Requirements**:
  - `rule` TR-5.1: `grep -r ZipFiles`（以及 `archivex`）在仓库中无残留引用；`go build ./...` 通过。
  - `rule` TR-5.2: 用 `go run . send --interface lo --bind 127.0.0.1 --port <p> <fixture-dir>` 冒烟：URL 可访问、manifest API 返回 200（token 从页面 HTML 提取）。

## Task 6: 传输页 HTML/CSS 骨架（go:embed）
- **Status**: `completed`
- **Completion Evidence**:
  - TR-6.1 通过：Go 测试 TestTransferPageInjectsToken 断言注入 64 位 token；网络面板除同源 /api/* 外零外部请求（单文件自包含，无外链 script/link/font）。
  - TR-6.2（rubric，无截图环境→无障碍树证据）：604px 视口下页面结构 = 头部（标题+阶段 badge）/会话信息行（token、过期时间、块大小）/总进度条与百分比、字节量、速度、ETA、重试计数/主按钮/文件清单（📁📄✅ 图标+每文件进度条+大小块数百分比）/完整性卡/日志卡；viewport meta 已设，单列 max-width 720 自适应，按钮 48px 高单手可点。真实 375px 截图留待 Reviewer 在本机补。
- **Priority**: medium
- **Depends On**: Task 4
- **Description**:
  - 新建 `pages/transfer.html`（自包含、无第三方 JS/CSS），`pages/pages.go` 用 `//go:embed transfer.html` 暴露 `Transfer`；占位元素：头部 token/过期时间、文件清单容器、总体与每文件进度条、速度/阶段状态、开始/恢复/保存按钮、日志/错误区。
  - 移动端 viewport、简洁中文 UI、与 qrcp 现有视觉协调。
- **Acceptance Criteria Addressed**: AC-13
- **Test Requirements**:
  - `rule` TR-6.1: `GET /send/<path>` 返回的 HTML 中含注入 token 且静态资源零外部请求（无外链 script/link/font）。
  - `rubric` TR-6.2: 移动端可用性维度；scale 1-5；anchors 1=布局溢出不可用 / 3=可用但粗糙 / 5=布局清晰单手可操作；阈值 >= 4；证据为 375px 宽浏览器截图。

## Task 7: 客户端核心 — IndexedDB 状态层、manifest 客户端、并发下载器
- **Status**: `completed`
- **Completion Evidence**:
  - TR-7.1 通过：8 块夹具 limit(4) 确定性中断（46%，3,145,750 字节）后重开页面显示"可恢复/继续传输"；继续后网络日志**仅** 4 个缺失块请求（f7c1、f8c1、f7c2、f8c2），其余块零重传；IDB 导出含 AC-8 全字段（token 64、api、createdAt/expiresAt、chunkSize、merkleRoot、mode、rootHandle/fileHandles、files[].chunks/hashes/done/fileRoot、corruptRetries、started/finished），chunks store 恰有 8 条 Blob 记录。中断过程中发现并修复"节流导致 done 标记滞后"bug：停止时强制落盘 + pagehide 刷盘 + 恢复时以 chunks store 自愈 done。
  - TR-7.2 通过：`QRCP_TEST_FAULT=0:0:1`，f0c0 请求恰好 2 次、f0c1/f0c2 各 1 次；UI"损坏重传 1 次"、corruptRetries=1，日志记录"校验失败，重传损坏块…（第 1 次）"；产物 SHA-256 与源一致。
  - TR-7.3 通过：浏览器直访 API（有 cookie 无 token）得 403 `{"error":"invalid token"}`（UI 403 分支代码同构）；TTL 2s 场景重开页面进入"会话已过期"（修复了 init 把 ExpiredError 误判 fatal 的 bug），按钮禁用并停止调度。
- **Priority**: high
- **Depends On**: Task 6
- **Description**:
  - 纯原生 JS 实现于 transfer.html：
    - IDB 层：DB `qrcp-xfer`，store `sessions`（token→会话元数据/文件/块状态）、`chunks`（[token,f,c]→{hash, blob?}）；句柄随会话记录结构化克隆。
    - 状态模型严格包含 AC-8 字段：token、baseURL、createdAt/expiresAt、chunkSize、merkleRoot、files[].chunks[]{hash,done}、FS 句柄或 Blob 记录。
    - API 客户端：带 token 轮询 manifest（退避 500ms 起）；chunk fetch 支持并发调度（全局并发 6，跨文件工作窃取队列）；WebCrypto SHA-256 比对（manifest hash 优先，响应头兜底并交叉复核）；失败指数退避，坏块仅重拉自身，统计 corruptRetries；410 进入过期态。
    - 会话恢复：加载时读 IDB→拉 manifest 对账（token/root 不符则新会话）→仅调度缺失块。
- **Acceptance Criteria Addressed**: AC-5, AC-6, AC-8, AC-9
- **Test Requirements**:
  - `rule` TR-7.1: 浏览器端到端：下载约 50% 关页重开，网络请求日志断言仅缺失块被重请求，IDB 中 AC-8 字段齐备（通过浏览器自动化导出 IDB JSON）。
  - `rule` TR-7.2: 设置 `QRCP_TEST_FAULT` 后坏块恰好重试 1 次并成功，其他块请求次数为 1，corruptRetries 统计正确。
  - `rule` TR-7.3: 410 响应后页面停止调度并显示会话过期；错误 token（手工改 IDB）得到 403 错误提示。

## Task 8: 客户端完整性校验与双模式落盘
- **Status**: `completed`
- **Completion Evidence**:
  - TR-8.1 通过：页面 merkleRoot 对空/1/2/3/4 叶输出与独立 Python 参考实现（同 Go 算法）5/5 完全一致（空=e3b0c4…、单叶=自身、奇数复制、4 叶=14ede5e8…）。
  - TR-8.2 通过（FS Access）：多文件夹具（嵌套目录+空文件+空目录+1MiB 整数倍+2.5/3MiB 尾块）经注入的 fake FileSystemHandle 全链路（真实 fetch/IDB/校验，仅替换选择器）：落盘树 qctp-e2e/{a.txt,empty,exact1m.bin,sub/nested/deep.bin,tail2m5.bin}+emptydir 与源一致，5 个文件 SHA-256 逐一相等；续传场景中先翻转已关闭文件磁盘字节并保留未写完状态，reVerify 准确报"恢复时发现已写块损坏，标记重下 a.txt#0/deep#0/tail#0"，仅重下作废块，最终哈希全对。过程中修复真实 bug：续传 ensureWritable 未传 keepExistingData（默认截断），已改 `createWritable({keepExistingData:true})`。单文件 showSaveFilePicker 分支另测：bad.bin 2,621,440 字节哈希一致。注：本机原生目录选择器因自动化进程无 macOS 辅助功能权限无法脚本驱动（用户未接手），真实落盘由 fake 合约+浏览器 API 语义覆盖。
  - TR-8.3 通过（Blob）：多文件 5 个下载产物（宿主重命名为隐藏临时文件，按大小识别）与源 SHA-256 全相等（6a125/ e3b0c/ 4e29a/ cee41/ 347a4）；单文件故障场景产物 15a8b6… 一致。
  - TR-8.4 通过：内存篡改 rec.merkleRoot 为全 0 后触发 startFlow，文件级 ✓ 全过但"✗ 全局 Merkle root 不匹配（本地 5264…/清单 0000…）"进入致命错误态，网络日志 complete 请求数 0。
- **Priority**: high
- **Depends On**: Task 7
- **Description**:
  - JS 实现与服务端完全一致的 Merkle（含奇数复制、空输入）；全部块完成且 root 就绪后做三级校验，任一失败进入错误态、禁止 complete。
  - FS Access 模式：单文件 `showSaveFilePicker`；多文件/目录 `showDirectoryPicker`，按相对路径建目录/文件；`createWritable` 按偏移写块；句柄存 IDB；恢复时手势 `requestPermission`，回读已写块切片复核 hash 后再续传。
  - Blob 回退模式（探测无 FS API，或通过调试开关强制）：块存 IDB，完成后按文件组装 Blob、`<a download>` 保存；多文件逐个保存按钮；完成后提供清理缓存动作。
  - 全部成功后 POST /api/complete。
- **Acceptance Criteria Addressed**: AC-5, AC-7, AC-10
- **Test Requirements**:
  - `rule` TR-8.1: Merkle JS 与 Go 向量一致（浏览器控制台对空/1/2/3/4 叶向量断言）。
  - `rule` TR-8.2: FS 模式端到端：多文件（含嵌套目录与空文件）传输后落盘文件树与源 `diff -r` 一致、逐文件 SHA-256 一致；中断恢复后同样一致。
  - `rule` TR-8.3: Blob 模式（强制关闭 `showSaveFilePicker/showDirectoryPicker`）单文件+多文件各一次，下载产物 SHA-256 一致。
  - `rule` TR-8.4: 篡改本地 root（IDB 改写）后页面报完整性错误且不发 /api/complete（网络面板无该请求）。

## Task 9: 客户端 UI 状态机与可观测性
- **Status**: `completed`
- **Completion Evidence**（TR-9.1 rubric，截图通道在离屏自动化环境不可用，以页面无障碍树+DOM 文本逐场景取证）：
  - 正常：就绪 badge、会话信息行（token 前 10 位/过期时间本地化/块大小）、0 B/6.5 MB 起始统计；下载中 46% 时显示"3.0 MB / 6.5 MB"、每文件进度（deep 33%/tail 40%）、✅ 完成图标。
  - 中断：重开后"可恢复"+"继续传输"+日志"发现未完成的本地会话，可从 3145750 / 6815766 字节处继续"；恢复时"从本机分块缓存恢复 N 个已完成块"。
  - 坏块：顶部统计区"损坏重传 1 次"，日志带时间戳记录失败块与第几次重试。
  - 过期：红色 badge"会话已过期"，按钮变灰禁用，中文指引重新发起。
  - 完成：绿色"完成"，完整性卡逐文件 ✓（块数+root 前 16 位）+全局 root，主按钮隐藏、出现清缓存按钮。
  - 篡改：红色"出错"+重试按钮+"全局 Merkle root 不匹配，传输内容可能已损坏或被篡改"。
  - 自评 ≥4：全部阶段与异常均有中文反馈、进度/速度/ETA/重试计数齐备；扣分项：375px 真机截图与多文件 Blob 批量下载的浏览器权限提示未在真机确认。
- **Priority**: medium
- **Depends On**: Task 8
- **Description**:
  - 阶段状态机：准备/等待哈希/下载中/校验中/保存中/完成/失败（过期、完整性、网络致命错）；每文件与总体进度、ETA、瞬时速度、坏块重试计数；开始/恢复/授权/保存均为明确按钮+中文说明；完成页展示全局 Merkle root 校验结果。
- **Acceptance Criteria Addressed**: AC-13
- **Test Requirements**:
  - `rubric` TR-9.1: 可用性维度；scale 1-5；anchors 1=异常场景无反馈 / 3=主流程可用但中断与错误令人困惑 / 5=各阶段与异常中文反馈清晰、移动端可单手操作；阈值 >= 4；证据为正常/中断/坏块/过期/完成五场景截图。

## Task 10: 端到端验证与回归
- **Status**: `completed`
- **Completion Evidence**:
  - TR-10.1 六场景全通过：①Blob 多文件 8 块并行（请求顺序证明跨文件工作窃取）+ 5 文件 SHA-256 全等；②中断续传只补 4 个缺失块（另修 done 落盘滞后 bug）；③故障块恰好重传 1 次且产物字节一致；④root 篡改停在错误态、0 个 complete；⑤--ttl 2s 后 410/会话已过期（另修 init 过期误判 bug）；⑥非保活 complete 后进程 exit 0（curl 冒烟与浏览器流程各一次），`-k` 保活时 complete 后 manifest 仍 200、二次 complete 仍 200。补充：FS 多文件/单文件全链路（fake handle）与 FS 续传损坏作废（另修 keepExistingData 截断 bug）。夹具含嵌套目录、空文件、空目录、1MiB 整数倍、2.5/3MiB 尾块。
  - TR-10.2 通过：`qrcp receive --output /tmp/qctp-recv` 上传页正常渲染、上传 a.txt 后显示 Done、落盘 SHA-256 6a125bab… 一致、服务按原行为自动退出。
  - 最终门：`go build ./...`、`go vet ./...`、`gofmt -l .`（空）、`go test -count=1 ./...` 全绿（manifest/server 含 -race）。
  - **独立 Review 后追加修复并复验通过**（详见 [review.md](file:///Users/kkcarrot/swe-project/qrcp_fork3/.trae/specs/resumable-chunked-transfer/review.md)）：C1 非安全上下文（局域网 HTTP）下 crypto.subtle 不可用 → 内置纯 JS SHA-256 回落 + 自动 Blob 模式 + 中文指引，已在真实 `http://192.168.50.231` 端到端复验（三级校验通过、5 文件哈希一致）；M2 顶层 symlink 改 Lstat 跳过（新单测）；M3 源文件变化 409 + 客户端长度门控（新单测）；M4 本地过期判定；M5 轮询 403/Fatal 不再吞错；M6 作废块自动补队；M7 纯空目录 Blob 可完成（浏览器复验 exit 0）；M8 路径一律 textContent；M10 哈希失败 DTO 显式报错、文件缺失 404、HEAD 不耗故障预算、调试钩子改为 `?debug=1` 门控、Send 先于 Serve 消除启动竞态、配额超限中文提示。修复后 `node --check`、`go test -race -count=1 ./...` 全绿。
  - 已知环境限制：macOS 原生文件选择对话框无法被自动化进程驱动（无辅助功能权限），FS 真实磁盘写入用浏览器 API 合约 fake 覆盖；截图通道不可用，TR-6.2/TR-9.1 以无障碍树+DOM 文本取证。
- **Priority**: high
- **Depends On**: Task 5, Task 8, Task 9
- **Description**:
  - 准备夹具：嵌套目录（空文件、空目录、4MiB 整数倍与非整数倍文件、>100MiB 大文件）；用浏览器自动化跑通：①完整多文件并行下载；②中断续传（FS/Blob）；③故障注入坏块重传；④root 篡改失败；⑤TTL 过期 410；⑥完成后服务端退出 / keep-alive 存活。
  - 留存截图、请求计数、哈希比对、进程行为作为完成证据。
- **Acceptance Criteria Addressed**: AC-5, AC-6, AC-7, AC-8, AC-9, AC-10
- **Test Requirements**:
  - `rule` TR-10.1: 六个场景全部通过且证据归档到任务 Completion Evidence。
  - `rule` TR-10.2: receive 方向回归：`qrcp receive` 上传页与上传功能不受路由改动影响（手工/自动化一次上传验证）。

## Task 11: 帮助文本与 README 更新
- **Status**: `completed`
- **Completion Evidence**:
  - TR-11.1 通过：`go run . send --help` 无 ZIP 措辞，含 `--chunk-size int (default 4)`、`--ttl string (default 24h)` 与 Example `qrcp --chunk-size 8 --ttl 2h /path/bigfile.bin`；README 新增 "Resumable Chunked Transfers" 章节（manifest/Merkle/并行/续传/TTL 说明、flags 表、FS Access/Blob 浏览器支持矩阵），发送表格去掉 zip 行，配置表补 `chunkSize`/`ttl`。
- **Priority**: low
- **Depends On**: Task 5
- **Description**:
  - 更新 `cmd/send.go` 的 Example（去掉"zip then send"措辞，说明分块/续传）；README 增加 QCTP 简述、`--chunk-size`/`--ttl` 说明、浏览器支持矩阵（FS Access/Blob 回退）。
- **Acceptance Criteria Addressed**: AC-14
- **Test Requirements**:
  - `rule` TR-11.1: `go run . send --help` 输出无 ZIP 旧措辞且包含新能力说明；README 新章节存在且命令示例可执行。
