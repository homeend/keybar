@echo off
rem Builds keybar.exe (no console window). Needs Go: https://go.dev/dl/
go build -ldflags "-H=windowsgui" -o keybar.exe . || exit /b 1
echo Built keybar.exe
echo   Run it:            keybar.exe
echo   Start at sign-in:  keybar.exe install
echo   Open the panel:    keybar.exe open
