@echo off
rem Switch the console to UTF-8, otherwise non-ASCII text in this file is
rem garbled on a GBK terminal.
chcp 65001 >nul 2>&1
rem LEVIATHAN supervisor start script (Windows)
rem
rem Usage:
rem   start.bat                  run in foreground (Ctrl-C to stop)
rem   start.bat -addr :9000      extra args are passed through to leviathan as-is
rem
rem It does exactly one thing: keep leviathan.exe running, and relaunch it
rem after a crash with a backoff delay.
rem
rem   0      user stopped it normally  -> exit the loop
rem   other  crash                     -> back off then relaunch (1->2->4… up to 60s)

setlocal enabledelayedexpansion
cd /d "%~dp0"

set "BIN=leviathan.exe"
if not exist "%BIN%" (
	echo [leviathan] executable not found: %BIN% 1>&2
	exit /b 1
)

set "MAX_DELAY=60"
set /a delay=1

:loop
"%BIN%" %*
set "code=!ERRORLEVEL!"

if "!code!"=="0" (
	echo [leviathan] exited normally
	exit /b 0
)

echo [leviathan] exited abnormally ^(code=!code!^), restarting in !delay!s 1>&2
rem `timeout` fails on a redirected console, so fall back to `ping` (an N-second
rem delay needs N+1 pings).
set /a pings=!delay!+1
ping -n !pings! 127.0.0.1 >nul 2>&1
set /a delay=!delay!*2
if !delay! gtr %MAX_DELAY% set /a delay=%MAX_DELAY%
goto loop
