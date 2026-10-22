@echo off
chcp 65001 >nul
cd /d "%~dp0"
echo 正在启动 Adobe Cookie 自动提取工具...
node server.cjs
pause
