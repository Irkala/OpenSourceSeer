@echo off
chcp 65001 >nul
cd /d "%~dp0"
if not exist output mkdir output
if not exist bin mkdir bin
echo 正在编译 gameserver ...
go build -ldflags "-s -w" -o output/gameserver.exe ./cmd/gameserver
if %errorlevel% neq 0 (
    echo 编译失败
    pause
    exit /b 1
)
echo.
echo 正在覆盖 bin\gameserver.exe（若正在运行服务器请先关闭，否则无法替换）
copy /y output\gameserver.exe bin\gameserver.exe
if %errorlevel% neq 0 (
    echo.
    echo 复制失败：请先关闭正在运行的 启动游戏服务器，再重新运行本 build.bat
    pause
    exit /b 1
)
echo.
echo 编译成功，已替换 bin\gameserver.exe
echo   - output\gameserver.exe
echo   - bin\gameserver.exe （启动游戏服务器.bat 会运行此文件）
pause
