@echo off
chcp 65001 >nul
cd /d "%~dp0"
for /f "tokens=5" %%a in ('netstat -ano ^| findstr ":18899 " ^| findstr "LISTENING"') do (
  taskkill /F /PID %%a >nul 2>&1
)
echo 正在启动 Adobe Cookie 自动提取工具...
if exist "C:\Program Files\nodejs\node.exe" (
  "C:\Program Files\nodejs\node.exe" server.cjs
) else (
  node server.cjs
)
pause
