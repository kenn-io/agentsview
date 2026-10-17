# 合并 upstream 的流程

本 fork（`origin`）在 upstream（`kenn-io/agentsview`）之外加了自己的功能，例如
Claude 活分支选择、Codex rollback、会话关系树、input outline、rolling build 和
自更新。本文说明如何把 upstream 的更新合进来，同时保留这些功能。

## 基本做法

- 用 `git merge`，不用 rebase。`origin/main` 已经公开推送，每次构建都会打
  `build-*` tag 并发布 rolling release，rebase 会改写这些历史。
- 每次合并只产生一个合并提交。它的 message 集中记录 fork 的每个功能是保留、重做
  还是丢弃，以及原因。
- 先在单独的合并分支上完成合并和验证，再把 `main` fast-forward 到合并分支并推送。
  推送 `main` 会立即发布 rolling release。推送合并分支只构建一个 `build-*` 快照，
  不更新 `latest`，可以先下载它的二进制测试；fast-forward 后，`main` 的构建会复用
  这个快照。fork 的 lint 和测试（`fork-checks.yml`）只在 PR 上运行；想在发布前让
  CI 跑一遍，可以先推送合并分支并开 PR，CI 通过后再 fast-forward。

## 步骤

1. 同步 fork：

   ```sh
   git checkout main
   git pull --ff-only origin main
   git fetch upstream
   ```

1. 查看 upstream 新增了什么：

   ```sh
   git log --no-merges --reverse --format='%h %ad %s' --date=short main..upstream/main
   ```

1. 建备份分支和合并分支，日期用当天：

   ```sh
   git branch backup/main-pre-merge-YYYYMMDD main
   git checkout -b merge/upstream-YYYYMMDD main
   git merge --no-ff --no-commit upstream/main
   ```

1. 按下文的规则解决冲突。

1. 按下文的清单验证。

1. 提交合并提交，推送合并分支，把 `main` fast-forward 过去并推送：

   ```sh
   git push -u origin merge/upstream-YYYYMMDD
   git checkout main
   git merge --ff-only merge/upstream-YYYYMMDD
   git push origin main
   ```

## 解决冲突的规则

### fork 功能和 upstream 重写撞在一起

把 fork 自上次合并以来的提交逐个归类：

- **保留**：在 upstream 新代码上重新实现，而不是保留旧代码。
- **丢弃**：upstream 已经有同等或更好的实现。
- **合并时附带的工作**：例如接口改名、生成代码、CI 修复。

在合并提交 message 里分这三类写清楚。

### upstream 测试编码了相反的语义

例如 upstream 在 rewind 后保留较早的分支，而 fork 跟随最后写入的活分支（和
Claude Code 显示一致）。这类测试改成 fork 的语义，并在测试里注释原因。

### 生成文件

`openapi.yaml`、`internal/apiclient/client.gen.go` 和
`frontend/src/lib/api/generated/**` 不手工合并：

1. 先解决手写的源文件，例如 `internal/server/huma_routes_*.go`、
   `internal/service/service.go`、`internal/servicehttp/http.go`。
1. 生成器要编译 `cmd/agentsview`，而它依赖 `client.gen.go`。先把
   `client.gen.go` 的冲突取两边并集，让它能编译。
1. 重新生成：

   ```sh
   cd frontend && npm run generate:api
   ```

1. `npm run check` 会再生成一次，结果有变化就失败，可用来确认生成文件已是最新。

### `dataVersion`（`internal/db/db.go`）

取 upstream 的值和注释。数据版本只有一个规则：数据库或 session 记录的版本低于当前
版本，就重新解析。没有按具体版本号做的定向迁移，所以取 upstream 更大的值时，fork
的旧库也会全部重新解析。只有 fork 自己改变了解析结果而 upstream 没有升版本时，才在
upstream 的值上加一。

### 接口新增方法

fork 给共享接口加的方法（例如 `db.Store.GetInputOutline`）要在 upstream 新增的
实现上补齐。编译失败会指出缺哪个，例如 `postgres.HostedStore`。

### `AGENTS.md`

保留 fork 自己加的规则：

- 任务路由表里 "Merging upstream into this fork" 一行，它指向本文。
- Git and Delivery 一节中"每次 push 后监控它触发的全部 workflow 运行"的规则。
  upstream 的 Pull Requests 一节有"除非用户要求，不要监控 GitHub Actions"的两条
  规则，与它相反，合并时不要把这两条加回来。

### CI workflow 的 runner

Namespace runner 只有 upstream 有，fork 里用它的 job 会一直排队，直到 GitHub
取消。fork 分两种情况处理：

- `ci.yml` 和 `cjk-fts.yml` 的每个 job 加了
  `if: github.repository == 'kenn-io/agentsview'`，在 fork 里跳过。
  `fork-checks.yml` 在 GitHub 托管 runner 上跑其中的 lint、Go、脚本、前端和 CJK
  FTS 检查。
- 桌面安装包（`desktop-artifacts.yml`、`desktop-artifacts-pr.yml` 和
  `.github/actions/build-desktop-artifact`）、`ci-macos-main.yml`、`fuzz.yml` 和
  `msys2-update-check.yml` 已经改用 GitHub 托管 runner。`msys2-update-check.yml`
  直接取自 upstream main，其余文件只移植了 upstream #2236 的改动。没有跟进
  #2242：它让桌面构建跳过 Windows 上的 Rust 测试，而 fork 跳过了 `ci.yml`，这些
  测试只在桌面构建里运行。
- `.github/actionlint.yaml` 保留，因为 `ci.yml` 和 `cjk-fts.yml` 仍引用 Namespace
  标签。

fork 只通过命令行安装 AgentsView，不发布桌面安装包，所以 `desktop-artifacts.yml`
和 `desktop-macos-main.yml` 的 `build` job 也加了同样的 `if` 条件，在 fork 里跳过。
PR 上的桌面构建通过 `desktop-artifacts-pr.yml` 调用 `desktop-artifacts.yml`，所以
一并跳过。这个跳过和 Namespace 无关；需要桌面安装包时，去掉这两处 `if` 即可，它们
已经使用托管 runner。

upstream 从 #2236（`65006001`）起把所有 job 改到了 GitHub 托管 runner。合并到包含
#2236 的 upstream 时：

1. 上面列出的 workflow 和 action 都取 upstream 的版本，`ci.yml` 和 `cjk-fts.yml`
   不再保留 `if` 条件；`.github/actionlint.yaml` 随 upstream 删除。
1. 只要 fork 仍不需要桌面安装包，就在 upstream 版本的 `desktop-artifacts.yml` 和
   `desktop-macos-main.yml` 上重新加回 `build` job 的 `if` 条件。
1. 删掉 `fork-checks.yml` 里和 upstream CI 重复的 job。整个 workflow 都重复时，
   删掉这个文件，并更新上文"基本做法"里提到它的那句话。

### 多语言文件

`frontend/messages/*.json` 的 key 集合必须和 `en.json` 完全一致，包括 upstream
新增的语言。

## 验证清单

1. Go 格式和静态检查：

   ```sh
   go fmt ./...
   go vet -tags fts5 ./...
   ```

1. Go 测试，至少覆盖 parser、db、sync、service、server、servicehttp、postgres、
   duckdb、clickhouse、update 和 `cmd/agentsview`。

1. 本机有些测试依赖较新的 git 或系统环境，会失败。在干净的 upstream worktree 上跑
   同一组测试，对比失败的测试名，确认合并没有新增失败：

   ```sh
   git worktree add --detach /tmp/upstream-wt upstream/main
   ```

   只在合并分支上失败的测试，通常是没有文本冲突的语义冲突，要逐个查清。例如
   2026-10-09 的合并中，upstream 的 Claude 增量 re-parse（#1972）和 fork 的 chunk
   合并规则互相影响，只有两个 sync 测试暴露了它。

1. 前端：

   ```sh
   cd frontend
   npm ci
   npm run i18n:compile
   npm run check
   npx vp test run
   ```

   单独重跑超时失败的测试文件。单独能通过的，一般是并行负载下的超时。

## 本机环境注意事项

- **sqlite3.h 找不到**：先运行 `make sqlite-vec-header`，再设置
  `CGO_CFLAGS="-O2 -g -I$PWD/.sqlite-include"`。
- **glibc 2.34 之前的头文件没有 `SYS_close_range`**：例如 Ubuntu 20.04，会让
  `internal/rawderive` 编译失败。只在本机的 `CGO_CFLAGS` 里加
  `-DSYS_close_range=436`（amd64 和 arm64 的调用号都是 436），不要改源码。rolling
  release 使用的 `manylinux_2_28` 镜像已经定义了它。
- **Go 模块或 npm 下载失败**：在配置了代理的用户 shell 里运行下载命令。
- **`vp fmt` 极慢或卡住**：oxfmt 默认按 CPU 核数起线程，在受限沙箱里可能卡住。改用
  `npx vp fmt --threads 4 <dir>`。

## 合并记录

| 日期       | 合并提交   | upstream 范围                        | 提交数 |
| ---------- | ---------- | ------------------------------------ | ------ |
| 2026-09-22 | `89c7d8d9` | v0.36 到 v0.44（到 `e5582c3b`）      | 591    |
| 2026-10-09 | `7cca11d3` | v0.44.0 之后的 main（到 `413277eb`） | 169    |
