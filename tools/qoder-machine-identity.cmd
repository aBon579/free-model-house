@echo off
rem Double-click shim for qoder-machine-identity.ps1 -- Windows opens .ps1 files in Notepad,
rem so this wrapper is what you actually double-click.
rem It runs with -ExecutionPolicy Bypass, adds -Reveal by default (Explorer opens with the
rem file selected), and pauses so the printed path stays readable instead of flashing away.
setlocal
for /f "tokens=2 delims=:." %%i in ('chcp') do set "OLDCP=%%i"
chcp 65001 >nul
set "PS=%SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe"
rem Windows PowerShell is only "available on demand" in future Windows releases, so fall back
rem to pwsh instead of dying with "file not found" on a machine that dropped it.
if not exist "%PS%" set "PS=pwsh"
"%PS%" -NoProfile -ExecutionPolicy Bypass -File "%~dp0qoder-machine-identity.ps1" -Reveal %*
set "RC=%ERRORLEVEL%"
if defined OLDCP chcp %OLDCP% >nul
echo.
pause
endlocal & exit /b %RC%
