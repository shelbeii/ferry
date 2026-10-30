@echo off
rem Double-click to build the Windows installer (see package.ps1 next to this file).
rem It asks whether to bump the version, runs the checks, builds, then selects the installer in Explorer.
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0package.ps1" -Ask -Open %*
echo.
pause
