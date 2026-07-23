#!/bin/bash
# AgentsView 开发模式启动脚本
# 双击此文件同时启动 Go 后端 + Vite 前端开发服务器
# 访问地址: http://localhost:5173

PROJECT_DIR="$(cd "$(dirname "$0")" && pwd)"
BINARY="$PROJECT_DIR/agentsview"
FRONTEND_DIR="$PROJECT_DIR/frontend"
GO_PID_FILE="$PROJECT_DIR/.agentsview-go.pid"
VITE_PID_FILE="$PROJECT_DIR/.agentsview-vite.pid"
GO_LOG="/tmp/agentsview-go.log"
VITE_LOG="/tmp/agentsview-vite.log"

# 检查二进制文件是否存在
if [ ! -f "$BINARY" ]; then
    osascript -e 'display dialog "agentsview 二进制文件不存在，请先运行 make build" buttons {"确定"} default button 1 with icon stop' > /dev/null 2>&1 || true
    exit 1
fi

# 检查前端依赖是否存在
if [ ! -d "$FRONTEND_DIR/node_modules" ]; then
    osascript -e 'display dialog "前端依赖未安装，请先运行 cd frontend && npm ci" buttons {"确定"} default button 1 with icon stop' > /dev/null 2>&1 || true
    exit 1
fi

# 如果已经有实例在运行，先停止
for pid_file in "$GO_PID_FILE" "$VITE_PID_FILE"; do
    if [ -f "$pid_file" ]; then
        OLD_PID=$(cat "$pid_file" 2>/dev/null) || true
        if kill -0 "$OLD_PID" 2>/dev/null; then
            kill "$OLD_PID" 2>/dev/null || true
            sleep 1
        fi
        rm -f "$pid_file"
    fi
done

# 启动 Go 后端（后台运行）
cd "$PROJECT_DIR"
nohup "$BINARY" serve > "$GO_LOG" 2>&1 &
GO_PID=$!
echo "$GO_PID" > "$GO_PID_FILE"

# 等待 Go 后端启动
echo "正在启动 Go 后端..."
for i in {1..10}; do
    if curl -s "http://localhost:8080/api/health" > /dev/null 2>&1; then
        echo "Go 后端已启动: http://localhost:8080"
        break
    fi
    sleep 1
done

# 启动 Vite 前端开发服务器（后台运行）
cd "$FRONTEND_DIR"
nohup npm run dev > "$VITE_LOG" 2>&1 &
VITE_PID=$!
echo "$VITE_PID" > "$VITE_PID_FILE"

# 等待 Vite 启动
echo "正在启动 Vite 前端..."
for i in {1..10}; do
    if curl -s "http://localhost:5173/" > /dev/null 2>&1; then
        echo "Vite 前端已启动: http://localhost:5173"
        break
    fi
    sleep 1
done

# 在浏览器中打开 Vite 开发服务器
open "http://localhost:5173"

# 显示通知
osascript -e 'display notification "开发模式已启动: http://localhost:5173" with title "AgentsView"' > /dev/null 2>&1 || true

echo ""
echo "========================================"
echo "AgentsView 开发模式已启动"
echo "  Go 后端:   http://localhost:8080"
echo "  Vite 前端: http://localhost:5173"
echo ""
echo "按回车键关闭两个服务..."
echo "========================================"
read -r

# 关闭服务
echo "正在关闭服务..."
kill "$GO_PID" 2>/dev/null || true
kill "$VITE_PID" 2>/dev/null || true
rm -f "$GO_PID_FILE" "$VITE_PID_FILE"

echo "AgentsView 已关闭"
