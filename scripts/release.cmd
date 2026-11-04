@echo off
rem Double-click to publish the current version on GitHub (see release.ps1 next to this file).
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0release.ps1" %*
set "release_exit=%ERRORLEVEL%"
echo.
pause
exit /b %release_exit%
