#!/bin/bash
# AgentsView 启动脚本
# 双击此文件即可启动 AgentsView 服务器并在浏览器中打开

set -e

PROJECT_DIR="$(cd "$(dirname "$0")" && pwd)"
BINARY="$PROJECT_DIR/agentsview"

# 检查二进制文件是否存在
if [ ! -f "$BINARY" ]; then
    osascript -e 'display dialog "agentsview 二进制文件不存在，请先运行 make build" buttons {"确定"} default button 1 with icon stop' > /dev/null 2>&1 || true
    exit 1
fi

# 启动服务器（后台运行）
"$BINARY" serve --background > /dev/null 2>&1 || true

# 等待服务器启动
sleep 2

# 获取实际端口（默认 8080）
PORT=8080

# 检查服务器是否真的在运行
if ! curl -s "http://localhost:$PORT/api/health" > /dev/null 2>&1; then
    # 尝试启动前台模式
    osascript -e 'display dialog "正在启动 AgentsView 服务器..." giving up after 2' > /dev/null 2>&1 || true
    nohup "$BINARY" serve > /dev/null 2>&1 &
    sleep 3
fi

# 在浏览器中打开
open "http://localhost:$PORT"
