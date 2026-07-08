#!/bin/bash
# AgentsView 启动脚本（生产模式）
# 双击此文件即可启动 AgentsView 服务器并在浏览器中打开
# 访问地址: http://localhost:8080

set -e

PROJECT_DIR="$(cd "$(dirname "$0")" && pwd)"
BINARY="$PROJECT_DIR/agentsview"
PID_FILE="$PROJECT_DIR/.agentsview.pid"
LOG_FILE="/tmp/agentsview.log"

# 检查二进制文件是否存在
if [ ! -f "$BINARY" ]; then
    osascript -e 'display dialog "agentsview 二进制文件不存在，请先运行 make build" buttons {"确定"} default button 1 with icon stop' > /dev/null 2>&1 || true
    exit 1
fi

# 如果已经有实例在运行，先停止
if [ -f "$PID_FILE" ]; then
    OLD_PID=$(cat "$PID_FILE" 2>/dev/null) || true
    if kill -0 "$OLD_PID" 2>/dev/null; then
        kill "$OLD_PID" 2>/dev/null || true
        sleep 1
    fi
    rm -f "$PID_FILE"
fi

# 启动服务器（后台运行）
nohup "$BINARY" serve > "$LOG_FILE" 2>&1 &
PID=$!
echo "$PID" > "$PID_FILE"

# 等待服务器启动（最多 10 秒）
echo "正在启动 AgentsView 服务器..."
for i in {1..10}; do
    if curl -s "http://localhost:8080/api/health" > /dev/null 2>&1; then
        break
    fi
    sleep 1
done

# 检查服务器是否真的在运行
if ! curl -s "http://localhost:8080/api/health" > /dev/null 2>&1; then
    osascript -e 'display dialog "AgentsView 服务器启动失败，请检查日志: /tmp/agentsview.log" buttons {"确定"} default button 1 with icon stop' > /dev/null 2>&1 || true
    rm -f "$PID_FILE"
    exit 1
fi

# 在浏览器中打开
open "http://localhost:8080"

# 显示通知
osascript -e 'display notification "生产模式已启动: http://localhost:8080" with title "AgentsView"' > /dev/null 2>&1 || true

echo "AgentsView 已启动: http://localhost:8080"
echo "PID: $PID"
echo "日志: $LOG_FILE"
