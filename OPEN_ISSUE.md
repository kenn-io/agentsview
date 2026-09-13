# OPEN_ISSUE

<!-- 活文档，只许这三节；每条 ≤3 行：一句现状 + 一句出路 + 指针（细节放 docs）；完成即删、不留 ✅ 编年史；全文超 ~60 行＝有条目该清 -->

## ⚠️ 待拍板

- 本文件是否随 PR #1745 分支推给上游：推则上游能读到这份内部流程笔记。出路：改放独立分支，或接受。（→ 本文件）

## 🚧 进行中

（暂无）

## 📌 待办与限制

- 原生投递未在真机验证：CI 只编译 Rust、前端测试打桩 `window.__TAURI__`，没有任何流程真正触发一次 OS 通知。出路：人工在 macOS 上走一次权限弹窗与投递。（→ `desktop/src-tauri/src/lib.rs:248`、`frontend/src/lib/stores/notifications.svelte.ts:108`）
- 浏览器不弹 OS 通知（设计如此）：web UI 已有实时更新，网页通知不在本功能范围。出路：接受。（→ PR #1745 描述）
- 本仓库 `docs/` 是上游文档站（zensical），没有 xyworkflow 约定的 `docs/human`/`docs/agent` 两层。出路：作为 fork 贡献不新建这两层，文档落在 PR 描述与代码注释。（→ 本文件）
- 重启时未投递的积压会永久丢失：新 Hub 以 `readyAt=now` 起步，早于它的行静默。出路：需持久化 resume 游标（属新增能力，非收敛）。（→ `internal/notify/hub.go:276`、`cmd/agentsview/main.go:426`）
- 排空积压期间新落地的「同行时间戳、id 更小」行会被跳过：需 >64 突发且排空超过 20s look-back。出路：同属游标加固，暂记。（→ `internal/notify/hub.go:264-278`）
- `Hub.Check` 注释称「可并发调用」，实为竞态安全但两次并发会重复通知（生产仅一个调用方）。出路：串行化，或把注释改成契约措辞。（→ `internal/notify/hub.go:200`）
- `NotificationState` 只拦语法错误：`null` / `{}` / `{"foo":1}` 仍读成零值（需外部篡改或字段改名才触发）。出路：暂记。（→ `internal/db/notifications.go:168-179`）
- `SaveNotificationState` 失败时该会话被跳过、游标仍前进：靠 20s look-back 兜住。出路：暂记。（→ `internal/notify/hub.go:250-255`）
- 测试与前端小瑕疵：`TestHubNeverFormatsTimestamps` 是源码文本断言（弱代理）；`hub_test.go` 的 `storeTimestamp` 与 db 侧 layout 常量重复（跨包不可避免）；`notifications_test.go` 多数用例用 `RFC3339Nano` 写列值（非生产格式）；前端 `notification` 监听器无形状校验、未知 kind 落到 turn_end 文案。（→ 对应文件）
