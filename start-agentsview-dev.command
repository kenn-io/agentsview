#!/bin/bash
# AgentsView 开发模式启动脚本
# 双击此文件同时启动 Go 后端 + Vite 前端开发服务器

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

# 清理之前的 PID 文件
rm -f "$PROJECT_DIR/.agentsview-go.pid" "$PROJECT_DIR/.agentsview-vite.pid"

# 用 AppleScript 打开一个新的 Terminal 窗口运行服务
osascript <<APPLESCRIPT
 tell application "Terminal"
    do script "cd '$PROJECT_DIR' && echo '启动 Go 后端...' && nohup '$BINARY' serve > /tmp/agentsview-go.log 2>&1 & echo \$! > '$PROJECT_DIR/.agentsview-go.pid' && sleep 2 && echo 'Go 后端已启动: http://localhost:8080' && cd '$FRONTEND_DIR' && echo '启动 Vite 前端...' && nohup npm run dev > /tmp/agentsview-vite.log 2>&1 & echo \$! > '$PROJECT_DIR/.agentsview-vite.pid' && sleep 3 && echo '' && echo '========================================' && echo 'AgentsView 开发模式已启动' && echo '  Go 后端:   http://localhost:8080' && echo '  Vite 前端: http://localhost:5173' && echo '' && echo '按回车键关闭两个服务...' && echo '========================================' && read -r && kill \$(cat '$PROJECT_DIR/.agentsview-go.pid' 2>/dev/null) 2>/dev/null; kill \$(cat '$PROJECT_DIR/.agentsview-vite.pid' 2>/dev/null) 2>/dev/null; rm -f '$PROJECT_DIR/.agentsview-go.pid' '$PROJECT_DIR/.agentsview-vite.pid'; echo 'AgentsView 已关闭'; exit"
    activate
 end tell
APPLESCRIPT

sleep 4

# 在浏览器中打开 Vite 开发服务器
open "http://localhost:5173"
