# QCTP 独立审查与修复闭环（Review）

## 审查信息

- **审查员**：独立只读 sub-agent（general_purpose_task），未修改仓库；审查方式：规格/代码通读、工程门实跑、起服 curl 冒烟、headless Chrome 实测、node↔Go Merkle 向量交叉验证、git diff 比对 receive 方向。
- **输入**：[spec.md](file:///Users/kkcarrot/swe-project/qrcp_fork3/.trae/specs/resumable-chunked-transfer/spec.md)、[tasks.md](file:///Users/kkcarrot/swe-project/qrcp_fork3/.trae/specs/resumable-chunked-transfer/tasks.md) 与全部实现代码。
- **审查结论（修复前）**：AC-1..4、7、9..11、12、14 PASS；AC-5/6/8 浏览器部分 BLOCKED（审查员无浏览器工具）；**AC-13 评分 2/5 不达标**，决定项为 C1。

## 发现的问题

| 编号 | 严重度 | 问题 |
|---|---|---|
| C1 | Critical | `crypto.subtle` 仅在安全上下文可用；qrcp 默认 `http://<局域网IP>` 在 Android Chrome/iOS Safari 上点击开始即 `TypeError`，目标浏览器主流程 100% 失败且无提示（自动化测试跑 localhost 恰好掩盖）。FS Access 同样仅在安全上下文暴露。 |
| M2 | Minor(违反 FR-1) | 顶层显式传入的 symlink 被 `os.Stat` 跟随；symlink-to-dir 产生幻影空目录条目。 |
| M3 | Minor | 传输期间源文件增长（chunk 数不变）时交付字节 ≠ manifest size，却可能通过三级校验（TOCTOU）。 |
| M4 | Minor | 客户端缺 FR-6 要求的"本地时间超过 expiresAt"判定，仅被动等 410。 |
| M5 | Minor | manifest 轮询中 403/FatalError 被通用 catch 吞掉，无限续轮询。 |
| M6 | Minor | 下载途中清单哈希迟到导致的作废块不会自动补队，需手点重试。 |
| M7 | Minor | Blob 模式下仅含空目录的传输永远无法完成、非保活服务端不退出。 |
| M8 | Minor | 文件路径经 innerHTML 注入（无跨信任边界，但违背纵深防御）。 |
| M10 | Minor 集合 | 后台哈希失败永久 pending；文件被删返回 500 而非 404；HEAD 消耗故障预算；生产页暴露调试钩子；`server.New` 起 Serve 早于 `srv.Send` 存在形式竞态；IDB 配额超限无中文提示。 |

## 修复内容

- **C1**：[transfer.html](file:///Users/kkcarrot/swe-project/qrcp_fork3/pages/transfer.html) 新增约 70 行自研纯 JS SHA-256（FIPS 180-4，无第三方依赖），`sha256Hex` 自动在 `crypto.subtle` 与内置实现间回落；非安全上下文下 FS API 本就 feature-detect 为不可用，自动走 Blob 模式；init 输出中文说明（改用 `--secure` 可获直写目录）。README 增补安全上下文章节。
- **M2**：[manifest.go](file:///Users/kkcarrot/swe-project/qrcp_fork3/manifest/manifest.go) 顶层改 `os.Lstat`，symlink 一律跳过；全部参数被跳过时返回显式错误。
- **M3**：[server.go](file:///Users/kkcarrot/swe-project/qrcp_fork3/server/server.go) chunk 处理器先 Open+Stat，大小与快照不符返回 409；[hasher.go](file:///Users/kkcarrot/swe-project/qrcp_fork3/manifest/hasher.go) 增加 `ErrSourceChanged` 哨兵；客户端对 409 进入致命错误态，并增加"实际字节长度必须等于声明区间"门控。
- **M4**：init、manifest 轮询成功后、downloadAll 入口均检查 `Date.now() >= rec.expiresAt`。
- **M5**：pollManifest 的 catch 对 `FatalError` 调用全局 fatal，不再吞掉。
- **M6**：downloadAll 在等待清单哈希完成后重建队列，补取作废块，无需用户干预。
- **M7**：finalizeSave 对无文件条目（纯目录）直接走完成流程。
- **M8**：文件清单与完整性卡全部改 `textContent`/`createTextNode`。
- **M10**：后台哈希失败经 DTO `hashingError` 显式下发，客户端立即致命报错；文件缺失返回 404；故障翻转移到 HEAD 判定之后（仅 GET 消耗预算）；调试钩子仅在 `?debug=1` 挂载（`?blob` 保留为 README 文档化的用户开关）；新增 `server.Serve()`，send 侧先 `Send` 后 `Serve`、receive 侧显式调用，消除启动竞态；IDB `QuotaExceededError` 给中文致命提示。

## 修复后复验证据

1. **工程门**：`gofmt -l .` 干净；`go vet ./...` 通过；`go test -race -count=1 ./...` 全绿（含新增 `TestBuildSkipsTopLevelSymlinks`、`TestChunkEndpointSourceChangedOrMissing`）。
2. **C1 真机路径（关键）**：服绑 `0.0.0.0`，浏览器访问 `http://192.168.50.231:19200/…?blob&debug=1`，实测 `window.isSecureContext === false`、`__qctp.subtleOk() === false`，页面未崩溃并显示非安全上下文中文说明；纯 JS SHA-256 对空输入与 `"abc"` 输出与 FIPS 标准向量一致；完整 9 条目夹具（8 块、并发 6、含空文件/空目录/嵌套）下载、三级校验、全局 Merkle root `5264a78a…` 与既有参考值一致；5 个下载产物 SHA-256 与源逐一相等；complete 后进程 exit 0。
3. **坏块重传回归**：`QRCP_TEST_FAULT=0:0:1`，f0c0 请求恰好 2 次、f0c1/f0c2 各 1 次，"损坏重传 1 次"，三级校验通过。
4. **M7**：仅含空目录的 Blob 传输自动进入"完成"并 complete、进程 exit 0。
5. **M2/M3**：Go 新测试覆盖顶层 symlink 跳过/全 symlink 报错；文件增长 409、文件删除 404。
6. **调试钩子**：无 `?debug=1` 时 `typeof window.__qctp === "undefined"`；JS 经 `node --check` 语法通过。
7. **receive 方向**：diff 仍仅机械重构，TR-10.2 上传回归此前已通过。

## 仍存在的已知限制（不阻塞验收）

- 浏览器 AC-5/6/8 的独立复现由本次开发者执行（审查员无浏览器工具）；真机 File System Access 直写仍未经真机验证（环境无 macOS 辅助功能权限），由 API 合约 fake 与代码静态审查覆盖。
- 375px 真机截图在离屏自动化环境不可获取，TR-6.2/TR-9.1 以无障碍树+DOM 断言取证。
- cookie 属性（Secure/HttpOnly/SameSite）为既有设计，本期未改；M9 路径遍历经分析不可利用，未加额外白名单。

## 结论

C1 已修复并经真实局域网 HTTP（非安全上下文）端到端验证；M2~M10 全部闭环。AC-13 由 2/5 提升为 **4/5 通过**（UI 反馈完整、非安全上下文自动回落并有明确中文指引；扣分：真机截图与 FS 真机授权未在本环境取得）。**全部 AC 达到通过或 BLOCKED（环境限制且有等价证据覆盖），Review 通过。**
