#!/bin/bash
# AgentsView 启动脚本（生产模式）
# 双击此文件即可启动 AgentsView 服务器并在浏览器中打开

set -euo pipefail

PROJECT_DIR="$(cd "$(dirname "$0")" && pwd)"
BINARY="$PROJECT_DIR/agentsview"

show_error() {
    osascript -e "display dialog \"$1\" buttons {\"确定\"} default button 1 with icon stop" > /dev/null 2>&1 || true
}

extract_loopback_url() {
    sed -nE 's#.*(http://127\.0\.0\.1:[0-9]+).*#\1#p' | tail -n 1
}

# 检查二进制文件是否存在
if [ ! -f "$BINARY" ]; then
    show_error "agentsview 二进制文件不存在，请先运行 make build"
    exit 1
fi

# 后台命令会等待子进程发布私有运行时记录，校验 PID，并使用 /api/ping
# 探测实际绑定地址。端口 0 让操作系统选择未占用的端口。
echo "正在启动 AgentsView 服务器..."
if ! LAUNCH_OUTPUT=$(
    "$BINARY" serve --background --host 127.0.0.1 --port 0 --no-browser 2>&1
); then
    printf '%s\n' "$LAUNCH_OUTPUT" >&2
    show_error "AgentsView 服务器启动失败，请在终端中运行 agentsview daemon status"
    exit 1
fi
printf '%s\n' "$LAUNCH_OUTPUT"

SERVER_URL=$(printf '%s\n' "$LAUNCH_OUTPUT" | extract_loopback_url)
if [ -z "$SERVER_URL" ]; then
    show_error "AgentsView 未返回可信的本机访问地址"
    exit 1
fi

# 使用受支持的健康端点进行最终检查。启用认证时，daemon status 会使用
# 私有配置中的令牌重复同一检查。
if ! curl --fail --silent "$SERVER_URL/api/ping" > /dev/null 2>&1 &&
    ! "$BINARY" daemon status > /dev/null 2>&1; then
    show_error "AgentsView 服务器未通过启动检查"
    exit 1
fi

open "$SERVER_URL"
osascript -e "display notification \"生产模式已启动: $SERVER_URL\" with title \"AgentsView\"" > /dev/null 2>&1 || true

echo "AgentsView 已启动: $SERVER_URL"
