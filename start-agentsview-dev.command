#!/bin/bash
# AgentsView 开发模式启动脚本
# 双击此文件同时启动 Go 后端 + Vite 前端开发服务器

set -euo pipefail
umask 077

PROJECT_DIR="$(cd "$(dirname "$0")" && pwd)"
BINARY="$PROJECT_DIR/agentsview"
FRONTEND_DIR="$PROJECT_DIR/frontend"
LAUNCH_DIR=""
VITE_PID=""
STARTED_BACKEND=0
KEEP_LOGS=0

show_error() {
    osascript -e "display dialog \"$1\" buttons {\"确定\"} default button 1 with icon stop" > /dev/null 2>&1 || true
}

extract_loopback_url() {
    sed -nE 's#.*(http://127\.0\.0\.1:[0-9]+).*#\1#p' | tail -n 1
}

cleanup() {
    if [ -n "$VITE_PID" ]; then
        kill "$VITE_PID" 2>/dev/null || true
        wait "$VITE_PID" 2>/dev/null || true
    fi
    if [ "$STARTED_BACKEND" -eq 1 ]; then
        "$BINARY" daemon stop > /dev/null 2>&1 || true
    fi
    if [ -n "$LAUNCH_DIR" ] && [ "$KEEP_LOGS" -eq 0 ]; then
        rm -f -- "$LAUNCH_DIR/vite.log" "$LAUNCH_DIR/backend-start.log"
        rmdir "$LAUNCH_DIR" 2>/dev/null || true
    fi
}
trap cleanup EXIT INT TERM

fail() {
    KEEP_LOGS=1
    show_error "$1"
    printf '%s\n' "$1" >&2
    if [ -n "$LAUNCH_DIR" ]; then
        printf '日志: %s\n' "$LAUNCH_DIR" >&2
    fi
    exit 1
}

# 检查二进制文件和前端依赖
if [ ! -f "$BINARY" ]; then
    fail "agentsview 二进制文件不存在，请先运行 make build"
fi
if [ ! -d "$FRONTEND_DIR/node_modules" ]; then
    fail "前端依赖未安装，请先运行 cd frontend && npm ci"
fi

TEMP_ROOT="${TMPDIR:-/tmp}"
LAUNCH_DIR=$(mktemp -d "${TEMP_ROOT%/}/agentsview-launch.XXXXXX") ||
    fail "无法创建安全的临时日志目录"
chmod 700 "$LAUNCH_DIR"
VITE_LOG="$LAUNCH_DIR/vite.log"
: > "$VITE_LOG"

# 让 Go 子进程发布并验证包含实际端口和 PID 的私有运行时记录。
echo "正在启动 Go 后端..."
if ! BACKEND_OUTPUT=$(
    "$BINARY" serve --background --host 127.0.0.1 --port 0 --no-browser 2>&1
); then
    printf '%s\n' "$BACKEND_OUTPUT" > "$LAUNCH_DIR/backend-start.log"
    fail "Go 后端启动失败"
fi
printf '%s\n' "$BACKEND_OUTPUT"
if printf '%s\n' "$BACKEND_OUTPUT" | grep -q '^agentsview running at '; then
    STARTED_BACKEND=1
fi

BACKEND_URL=$(printf '%s\n' "$BACKEND_OUTPUT" | extract_loopback_url)
if [ -z "$BACKEND_URL" ]; then
    fail "Go 后端未返回可信的本机访问地址"
fi
if ! curl --fail --silent "$BACKEND_URL/api/ping" > /dev/null 2>&1 &&
    ! "$BINARY" daemon status > /dev/null 2>&1; then
    fail "Go 后端未通过启动检查"
fi
echo "Go 后端已启动: $BACKEND_URL"

# Vite 在随机端口上绑定明确的 IPv4 回环地址。它的实际 URL 只从当前
# 子进程写入的私有日志中读取，并在打开浏览器前同时校验 PID 和 HTTP。
echo "正在启动 Vite 前端..."
cd "$FRONTEND_DIR"
nohup env VITE_API_TARGET="$BACKEND_URL" \
    npm run dev -- --host 127.0.0.1 --port 0 --strictPort \
    > "$VITE_LOG" 2>&1 &
VITE_PID=$!

VITE_URL=""
for _ in {1..50}; do
    if ! kill -0 "$VITE_PID" 2>/dev/null; then
        fail "Vite 前端进程提前退出"
    fi
    VITE_URL=$(extract_loopback_url < "$VITE_LOG")
    if [ -n "$VITE_URL" ] &&
        curl --fail --silent "$VITE_URL/" > /dev/null 2>&1; then
        break
    fi
    VITE_URL=""
    sleep 0.2
done

if [ -z "$VITE_URL" ]; then
    fail "Vite 前端启动超时"
fi

open "$VITE_URL"
osascript -e "display notification \"开发模式已启动: $VITE_URL\" with title \"AgentsView\"" > /dev/null 2>&1 || true

echo ""
echo "AgentsView 开发模式已启动"
echo "  Go 后端:   $BACKEND_URL"
echo "  Vite 前端: $VITE_URL"
echo ""
echo "按回车键关闭两个服务..."
read -r || true

echo "正在关闭服务..."
