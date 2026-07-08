#!/bin/bash
# AgentsView 开发模式启动脚本
# 双击此文件同时启动 Go 后端 + Vite 前端开发服务器

set -e

PROJECT_DIR="$(cd "$(dirname "$0")" && pwd)"
BINARY="$PROJECT_DIR/agentsview"
FRONTEND_DIR="$PROJECT_DIR/frontend"

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

# 启动 Go 后端（后台运行）
nohup "$BINARY" serve > /dev/null 2>&1 &
GO_PID=$!

# 启动 Vite 前端开发服务器（后台运行）
cd "$FRONTEND_DIR"
nohup npm run dev > /dev/null 2>&1 &
VITE_PID=$!

# 保存 PID 到文件，方便后续关闭
echo "$GO_PID" > "$PROJECT_DIR/.agentsview-go.pid"
echo "$VITE_PID" > "$PROJECT_DIR/.agentsview-vite.pid"

# 等待 Vite 启动
sleep 3

# 在浏览器中打开 Vite 开发服务器
open "http://localhost:5173"

# 显示提示
osascript -e 'display notification "Go 后端: http://localhost:8080 | Vite 前端: http://localhost:5173" with title "AgentsView 已启动"' > /dev/null 2>&1 || true

# 保持脚本运行，等待用户按回车关闭
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
kill "$GO_PID" 2>/dev/null || true
kill "$VITE_PID" 2>/dev/null || true
rm -f "$PROJECT_DIR/.agentsview-go.pid" "$PROJECT_DIR/.agentsview-vite.pid"

echo "AgentsView 已关闭"
