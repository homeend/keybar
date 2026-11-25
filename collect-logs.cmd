@echo off
rem Copies Keybar's logs into .\logs (git-ignored), named after this computer.
setlocal
set SRC=%LOCALAPPDATA%\keybar
set DST=%~dp0logs
if not exist "%DST%" mkdir "%DST%"
set FOUND=0
for %%F in (keybar.log keybar.prev.log) do (
  if exist "%SRC%\%%F" (
    copy /y "%SRC%\%%F" "%DST%\%COMPUTERNAME%-%%F" >nul
    echo Copied %%F to logs\%COMPUTERNAME%-%%F
    set FOUND=1
  )
)
if "%FOUND%"=="0" echo No logs found in %SRC% - has Keybar run on this computer?
endlocal
