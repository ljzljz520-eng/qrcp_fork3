# qrcp 可恢复分块传输协议（QCTP v1） - 产品需求文档

## Overview
- **Summary**: 将 qrcp 发送方向"多文件/目录预先打包成单个 ZIP、浏览器原生单连接下载"的模式，改造为基于 manifest + 固定大小 chunk 的可恢复分块传输协议：服务端按需读取文件字节、提供清单与分块 API；手机端 Web 页面负责并行下载、逐块 SHA-256 校验、损坏块重传、Merkle 完整性验证，并把传输状态持久化到 IndexedDB，支持中断后续传。
- **Purpose**: 解决大文件/目录传输中的四个痛点：① 发送前必须等待完整 ZIP 生成且占用临时空间；② 网络切换/页面关闭后下载前功尽弃；③ 传输损坏无法发现、只能整体重来；④ 多文件无法并行、完成后无完整性保证。
- **Target Users**: 通过手机浏览器（iOS Safari、Android Chrome）或桌面浏览器从运行 qrcp 的主机接收大文件/目录的用户。

## Goals
- 服务端不再生成 ZIP：直接遍历文件树生成 manifest，chunk 按需从源文件按偏移读取。
- 中断后继续：页面刷新/关闭重开/网络中断后，只下载未完成的 chunk。
- 只重传损坏块：每个 chunk 做 SHA-256 校验，失败块自动重试，不影响已完成块。
- 多文件并行：下载器跨文件维持固定并发度。
- 传输后完整性验证：逐块 hash → 每文件 Merkle root → 全局 Merkle root，三级验证全部通过才宣告完成。
- 客户端持久化保存：已完成 chunk 集合、每 chunk SHA-256、总体 Merkle root、临时文件位置、服务器 token 与过期时间。

## Non-Goals
- 不改造接收方向（手机上传到电脑，`qrcp receive`）。
- 不支持服务端进程重启后的跨进程续传（新进程生成新的随机路径与 token）。
- 不支持 symlink、设备文件等非常规文件（symlink 在遍历中跳过）。
- iOS Safari 等不支持 File System Access API 的浏览器上不做目录结构还原，回退为逐文件 Blob 下载。
- 不扩展 `qrcp config` 向导（wizard）界面；新参数通过命令行 flag 与配置文件键提供。
- 不做断点上传、P2P/中继、多客户端会话。

## Background & Context
- 现状：[body/payload.go](file:///Users/kkcarrot/swe-project/qrcp_fork3/body/payload.go) 的 `FromArgs` 在多参数或含目录时调用 [util/util.go](file:///Users/kkcarrot/swe-project/qrcp_fork3/util/util.go#L36-L83) 的 `ZipFiles` 在系统临时目录生成完整 ZIP；[server/server.go](file:///Users/kkcarrot/swe-project/qrcp_fork3/server/server.go#L195-L234) 的 `/send/<path>` 处理器仅设置 `Content-Disposition` 后 `http.ServeFile`，手机端依赖浏览器原生下载，无会话层、无校验、无续传状态。
- 现有会话机制：`/send/<path>` 路径本身是随机密钥，且对 Mozilla UA 用一次性 cookie 锁定第一客户端、用 WaitGroup 计数在请求排空后关闭服务端。
- 页面资源以 Go 字符串内嵌在 [pages/pages.go](file:///Users/kkcarrot/swe-project/qrcp_fork3/pages/pages.go)（Upload/Done 两页）。Go 版本 1.21，可用 `go:embed`。
- 决策 D1（用户未明确选择，按推荐默认，审批可改）：**单文件/多文件/目录全部走新协议**，移除服务端预打包 ZIP 代码路径。
- 决策 D2（同上）：**落盘采用双策略**——支持 File System Access API 的浏览器（Chromium 桌面/Android）用文件/目录句柄按偏移写盘并持久化句柄；不支持时回退 IndexedDB 缓存 chunk + 组装 Blob 下载。

## Functional Requirements

### FR-1：Manifest 构建（服务端）
- 启动发送时遍历输入参数，生成 manifest：协议版本、会话 token、创建/过期时间、chunk 大小、文件条目列表。
- 路径规则：单文件条目名为其 basename；单目录以目录 basename 为根递归；多参数时各参数取 basename（目录递归加前缀）；顶层重名按 `name(2)` 规则去重；空文件保留为 0-chunk 条目；空子目录以目录条目保留；symlink 跳过。
- 每个文件条目包含：index、相对路径、类型（file/dir）、字节大小、权限位、修改时间、chunk 数、chunk hash 列表、文件 Merkle root。
- 整个过程不创建任何 ZIP/临时打包文件。

### FR-2：哈希与 Merkle（服务端）
- chunk hash = 小写十六进制 `SHA-256(chunk 原始字节)`。
- 文件 Merkle root：以有序 chunk hash 为叶节点；奇数节点复制最后一个节点配对；空文件 root = `SHA-256("")`。
- 文件绑定叶 = `SHA-256(以 \n 连接的: "qctp-v1", path, size(十进制), mode(十进制), modTime(Unix 秒), fileMerkleRoot)`，将路径/元数据绑定进全局树。
- 全局 Merkle root = 对全部文件绑定叶做同一 Merkle 运算；无文件时为 `SHA-256("")`。
- 哈希在发送启动时由后台 goroutine 顺序预计算并缓存；chunk 请求到达而该块尚未预计算时，同步即算即缓存（hash-while-serve，不重复读盘）；manifest 接口在单文件哈希未完成时其 `chunkHashes` 为 `null`，全部完成后填充 `merkleRoot` 与 `hashingDone: true`。

### FR-3：HTTP 协议（服务端）
- 所有端点挂在现有随机密钥路径 `/send/<path>` 之下，沿用现有 cookie 锁定第一客户端的机制：
  - `GET /send/<path>`：返回传输页 HTML（token 注入页面）。
  - `GET /send/<path>/api/manifest`：返回 manifest JSON（支持重复轮询）。
  - `HEAD|GET /send/<path>/api/chunk?f=<fileIndex>&c=<chunkIndex>`：返回该 chunk 原始字节；响应头含 `X-Chunk-Sha256`、`X-Chunk-Index`、`Content-Length`、`Cache-Control: no-store`；读取字节区间恒为 `[c*chunkSize, min((c+1)*chunkSize, size))`。
  - `POST /send/<path>/api/complete`：客户端完成全部验证与保存后上报。
- 鉴权：除页面外所有 `/api/` 请求必须携带 `X-QCTP-Token`（或 `Authorization: qrcp <token>`），缺失/不匹配返回 403；file/chunk 索引非法返回 404；会话过期返回 `410 Gone` 且 JSON body 说明原因。
- 每个 chunk 请求独立打开文件句柄并 seek 读取，支持跨文件高并发。
- 非 keep-alive 模式下，收到 `/api/complete` 后关闭服务端（替代现有 WaitGroup 排空启发式）；keep-alive 模式保持运行。
- 提供仅测试用故障注入：环境变量 `QRCP_TEST_FAULT=f:c:n` 使文件 f 的 chunk c 的前 n 次响应首字节被翻转，用于端到端验证坏块重传（生产无该变量时零副作用）。

### FR-4：会话与参数
- 新增 flag/配置：`--chunk-size`（单位 MiB，默认 4）、`--ttl`（Go duration 字符串，默认 24h）。
- token 为密码学随机 32 字节十六进制；服务端记录创建时间与过期时间，过期后所有 API 返回 410。

### FR-5：传输页与下载器（客户端）
- 打开 `/send/<path>` 显示传输页：文件清单、每文件与总体进度、瞬时速度、当前阶段（准备/哈希等待/下载中/校验中/保存中/完成/失败）。
- 下载器跨所有文件维持固定并发（默认 6）调度 chunk；每块下载后立即用 WebCrypto 计算 SHA-256：优先与 manifest 中已知 hash 比对，未知时与响应头 `X-Chunk-Sha256` 比对，待 manifest 补齐后交叉复核；失败按指数退避重试，超过次数标记坏块并仅重拉该块，页面展示坏块/重传统计。
- manifest 轮询直到 `hashingDone`，随后执行三级完整性校验：逐块 hash、文件 Merkle root、全局 Merkle root；任一不通过则禁止完成并给出明确错误（含不匹配的文件/块）。
- 中断恢复：页面加载时先从 IndexedDB 恢复会话状态（按 token 区分），与最新 manifest 对账（token/root 不一致按新会话处理），只下载缺失块；恢复已有 File System Access 句柄需经用户点击手势重新授权，恢复时回读已写区块复核 hash，损坏块重新下载。

### FR-6：客户端持久化（IndexedDB）
- 数据库 `qrcp-xfer`，按 token 隔离，必须持久化：会话 token、服务端基础 URL、创建/过期时间、chunkSize、全局 Merkle root、每文件路径/大小/每块 hash 与完成状态；FS 模式持久化文件/目录句柄（"临时文件位置"），Blob 模式持久化每块 Blob 数据。
- 会话过期（API 410 或本地时间超过 expiresAt）时页面明确提示"会话已过期"，停止调度。

### FR-7：落盘
- FS Access 模式：单文件用 `showSaveFilePicker`（suggestedName 为文件名），多文件/目录用 `showDirectoryPicker` 并按相对路径创建子目录与文件；用可写流按偏移写入；句柄与每块完成状态存 IndexedDB。
- Blob 回退模式：全部块下载并校验后按文件顺序组装 Blob 触发下载；多文件逐个提供保存动作。完成后可清理该会话的 chunk Blob 缓存。

## Non-Functional Requirements
- **NFR-1 性能**：服务端 chunk 读取为有界内存（每次只读一个 chunk 到缓冲），manifest 构建与哈希不在堆中累积文件内容；客户端默认并发 6、chunk 4MiB，在百 MB～GB 级文件上工作正常。
- **NFR-2 可靠性**：网络错误、服务器瞬时不可达、标签页意外关闭均不丢失已完成状态；恢复后不产生重复/错位字节。
- **NFR-3 安全**：token 用 crypto/rand 生成；沿用 HTTPS、随机路径、cookie 锁定；不引入新的第三方 JS 依赖（纯原生 WebCrypto/fetch/IndexedDB/File System Access API）。
- **NFR-4 可维护性**：服务端协议逻辑独立成包；前端为单个静态 HTML（`go:embed`），无前端构建工具链；Go 单元测试覆盖 manifest、Merkle、HTTP API。
- **NFR-5 兼容**：macOS/Linux/Windows 服务端行为一致；前端在当前版 Chrome/Edge 与 iOS Safari 上可完成各自支持模式下的完整流程。

## Constraints
- **Technical**: Go 1.21；cobra/viper flag 与配置体系；不能引入 npm/webpack 工具链；`go test` 为唯一测试框架；浏览器自动化验证使用当前环境可用的浏览器工具。
- **Business**: 保持 qrcp 现有 QR + 随机路径的交互形态；receive 方向行为不变。
- **Dependencies**: 仅使用 Go 标准库实现 Merkle/会话/HTTP API；前端仅浏览器原生 API。

## Assumptions
- 用户在同一局域网/可达网络内，传输层损坏为偶发错误而非主动攻击；Merkle 主要防御意外损坏（hash 与数据同通道交付，TLS 可选）。
- 移动 Chromium 与桌面 Chromium 支持 File System Access API；Android Chrome 对 `showDirectoryPicker` 的支持差异由 Blob 回退兜底。
- 后台预哈希可能对超大文件耗时较长；期间客户端可凭响应头 hash 先行下载，全局 root 在哈希完成后才做最终门控。
- manifest JSON 体积可接受（每 4MiB chunk 约 64 字符 hash，即约 16KB/GB）。
- D1/D2 默认决策如审批时被修改，本规格与任务计划相应回炉。

## Acceptance Criteria

### AC-1: 服务端不再预打包 ZIP
- **Type**: `rule`
- **Given**: 执行 `qrcp send <dir>` 或 `qrcp send a.bin b.bin`
- **When**: 服务启动并产生 manifest
- **Then**: 进程不调用任何 zip 打包逻辑、不在临时目录创建 zip；manifest 条目相对路径符合 FR-1 路径规则（含顶层目录名、空文件、空目录、symlink 跳过、重名去重）
- **Pass Condition**: 代码中 send 路径无 `ZipFiles` 调用；单元测试断言临时目录无新增 `*.zip`，且对文件树夹具生成的条目集合与预期完全一致
- **Evidence**: manifest 包单元测试输出；`grep` 发送路径无 zip 调用

### AC-2: Manifest 接口字段完整且最终收敛
- **Type**: `rule`
- **Given**: 一个含多文件（含跨 chunk 大文件与空文件）的发送会话
- **When**: 轮询 `GET /api/manifest`
- **Then**: 返回字段包含 protocol、token、createdAt、expiresAt、chunkSize、hashingDone、merkleRoot、files[]（index/path/type/size/mode/modTime/chunks/chunkHashes/merkleRoot）；最终 `hashingDone=true` 且每块 hash 与独立用 sha256sum 计算结果一致
- **Pass Condition**: httptest 测试逐字段断言，并独立计算全部 chunk hash 与 root 比对一致
- **Evidence**: `go test ./manifest/... ./server/...`

### AC-3: Chunk API 字节正确、头部正确、错误码正确
- **Type**: `rule`
- **Given**: 有效 token 与已知大小的文件
- **When**: 任意顺序请求全部 chunk（含 HEAD）、请求越界 c、非法 f、不带 token
- **Then**: 200 响应字节严格等于源文件对应区间；`X-Chunk-Sha256` 等于 body 的 SHA-256；越界/非法索引 404；无 token 403；过期会话 410
- **Pass Condition**: httptest 表驱动测试覆盖上述全部情形且通过
- **Evidence**: server 包测试输出

### AC-4: 服务端高并发读取正确
- **Type**: `rule`
- **Given**: 大于 100MB 的文件与多个小文件
- **When**: 8 并发随机顺序拉取全部 chunk 并在客户端（测试程序中）按偏移拼接
- **Then**: 拼接结果字节数与 SHA-256 与源文件完全一致
- **Pass Condition**: 并发集成测试通过
- **Evidence**: server 包并发测试输出

### AC-5: 客户端中断续传（FS 与 Blob 两种模式）
- **Type**: `rule`
- **Given**: 多文件传输进行到约 50%（含跨 chunk 文件）
- **When**: 关闭标签页后重新打开同一 URL（FS 模式重新授权句柄）
- **Then**: 网络面板显示仅未完成 chunk 被请求；最终落盘文件树与源一致（逐文件 `shasum -a 256` 相等，含目录结构）；Blob 模式（强制关闭 FS API）同样成立
- **Pass Condition**: 浏览器端到端脚本在两种模式下各完成一次中断-恢复，哈希比对全部相等，且恢复后请求数 ≤ 缺失 chunk 数（允许少量元数据请求）
- **Evidence**: 浏览器自动化截图、请求计数日志、shasum 输出

### AC-6: 损坏块自动检测且仅重传损坏块
- **Type**: `rule`
- **Given**: 设置 `QRCP_TEST_FAULT=0:2:1`（0 号文件第 2 块首次响应被篡改）
- **When**: 客户端下载该文件
- **Then**: UI 出现一次校验失败/重试记录；第 2 块被第二次请求且成功；其余块不重复请求；最终文件与源 SHA-256 一致
- **Pass Condition**: 浏览器端到端观察到重试计数=1、第 2 块请求次数=2、其余块=1，最终哈希一致
- **Evidence**: 浏览器自动化请求日志与哈希输出

### AC-7: 三级完整性校验是完成门控
- **Type**: `rule`
- **Given**: 传输页处于下载/校验阶段
- **When**: 全部 chunk 完成、manifest root 就绪
- **Then**: 只有逐块 hash、文件 Merkle root、全局 Merkle root 全部通过时 UI 才进入"完成"并允许 POST /api/complete；通过测试钩子篡改服务端 root 后，页面显示完整性错误且不上报 complete
- **Pass Condition**: Merkle 向量单测（空/1/2/3 节点、奇数复制）通过；浏览器端篡改 root 场景停在错误态
- **Evidence**: Go 单测 + 浏览器自动化截图

### AC-8: 客户端持久化状态齐备
- **Type**: `rule`
- **Given**: 一个进行中的传输会话
- **When**: 检查 IndexedDB `qrcp-xfer`
- **Then**: 可读到 token、URL、expiresAt、chunkSize、全局 merkleRoot、每文件每块 hash 与完成标记，以及临时文件位置（FS 模式的文件/目录句柄，或 Blob 模式的块 Blob 记录）；刷新页面后进度从持久化状态恢复
- **Pass Condition**: 浏览器自动化读取 IndexedDB 断言字段齐备；刷新后进度条不重置
- **Evidence**: IndexedDB 内容导出、刷新前后截图

### AC-9: Token/TTL 鉴权强制生效
- **Type**: `rule`
- **Given**: `--ttl` 设为短时长的会话
- **When**: 过期后请求任意 /api/ 端点
- **Then**: 返回 410 与 JSON 错误；页面停止调度并显示"会话已过期"；错误 token 返回 403
- **Pass Condition**: httptest 403/410 测试通过；浏览器端过期提示可见
- **Evidence**: Go 测试输出 + 浏览器截图

### AC-10: 完成信号驱动服务端生命周期
- **Type**: `rule`
- **Given**: 非 keep-alive 的发送会话
- **When**: 客户端校验保存完成并 POST /api/complete
- **Then**: 服务端进程在短时间内退出；keep-alive 模式下不退出、可服务重新打开的页面
- **Pass Condition**: 两种模式各执行一次，进程行为符合预期
- **Evidence**: 进程退出码/存活检查日志

### AC-11: 工程质量门
- **Type**: `rule`
- **Given**: 全部实现完成
- **When**: 执行 `go build ./...`、`go vet ./...`、`go test ./...`
- **Then**: 全部成功通过
- **Pass Condition**: 三条命令退出码均为 0
- **Evidence**: 命令输出

### AC-12: 协议设计质量
- **Type**: `rubric`
- **Dimension**: 协议自洽性、边界完备性（空文件/空目录/奇数 Merkle/越界/并发/过期）、可演进性（版本字段）
- **Scale**: 1-5
- **Anchors**: 1 = 协议有歧义或边界未定义；3 = 主流程自洽但 2 处以上边界靠隐含行为；5 = 字段、状态机、错误码、边界均显式定义且经测试锁定
- **Pass Threshold**: >= 4
- **Evidence**: 规格与实现代码、API 测试集合

### AC-13: Web 客户端可用性
- **Type**: `rubric`
- **Dimension**: 进度/速度/阶段反馈、续传手势与提示、错误可恢复性、移动端布局
- **Scale**: 1-5
- **Anchors**: 1 = 白屏或失败无提示；3 = 能完成主流程但中断/错误场景令人困惑；5 = 每个阶段与异常都有清晰中文反馈，移动端单手可操作
- **Pass Threshold**: >= 4
- **Evidence**: 浏览器自动化多场景截图与交互记录

### AC-14: 代码质量与一致性
- **Type**: `rubric`
- **Dimension**: 包职责清晰、与现有代码风格一致、无新增 JS 依赖/构建链、测试可读性
- **Scale**: 1-5
- **Anchors**: 1 = 逻辑揉在 server.go、巨型函数；3 = 有分包但边界模糊；5 = manifest/server/pages 职责清晰、命名一致、测试独立
- **Pass Threshold**: >= 4
- **Evidence**: 代码审查与目录结构

## Open Questions
- [x] D1/D2 默认决策（全部走新协议、FS Access + Blob 双策略）已获用户审批确认。
- [ ] 若将来需要"服务端重启后续传"，需把随机路径/token 持久化到磁盘，本期不做。
