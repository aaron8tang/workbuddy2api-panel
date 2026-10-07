@echo off
setlocal enabledelayedexpansion
rem ============================================================
rem update-docker.bat - rebuild the image from local sources and swap the container
rem (Windows-native twin of update-docker.sh; run directly in cmd)
rem
rem Data safety: auths\, data\ and config.json are host bind mounts,
rem so rebuilding the container never touches account-pool data.
rem
rem Usage: update-docker.bat [image:tag]    (default: workbuddy2api-panel:local)
rem Prefer running from a cmd window (double-click closes when done).
rem ============================================================

set "IMAGE=%~1"
if "%IMAGE%"=="" set "IMAGE=workbuddy2api-panel:local"
set "NAME=workbuddy2api"
set "PORT=7863"
set "ROOT=%~dp0"
if "%ROOT:~-1%"=="\" set "ROOT=%ROOT:~0,-1%"

docker version >nul 2>&1
if errorlevel 1 (
  echo [ERR] docker not available - not installed or Docker Desktop not running
  exit /b 1
)
if not exist "%ROOT%\config.json" (
  echo [ERR] missing %ROOT%\config.json - a missing mount file becomes a directory and breaks startup
  echo       create it first: copy config.example.json config.json
  exit /b 1
)
if not exist "%ROOT%\auths\" (
  echo [ERR] missing %ROOT%\auths\ directory
  exit /b 1
)

rem remember the image of the current container (empty = no container, no rollback path)
set "OLD_IMAGE="
for /f "usebackq delims=" %%v in (`docker inspect -f "{{.Config.Image}}" %NAME% 2^>nul`) do set "OLD_IMAGE=%%v"

echo [1/4] building image %IMAGE% ...
docker build -t %IMAGE% "%ROOT%"
if errorlevel 1 (
  echo [ERR] build failed - old container untouched, service keeps running
  exit /b 1
)

echo [2/4] removing old container ...
docker rm -f %NAME% >nul 2>&1

echo [3/4] starting new container ...
call :start %IMAGE%
if errorlevel 1 (
  echo [ERR] failed to start new container
  call :rollback
  exit /b 1
)

echo [4/4] waiting for readiness (up to 60s) ...
set "CODE=000"
for /l %%i in (1,1,30) do (
  if "!CODE!"=="000" (
    for /f "usebackq" %%c in (`curl -s -o nul -m 3 -w "%%{http_code}" http://localhost:%PORT%/healthz 2^>nul`) do set "CODE=%%c"
    if "!CODE!"=="000" timeout /t 2 /nobreak >nul
  )
)
if "!CODE!"=="200" goto :healthy
if "!CODE!"=="503" goto :healthy_nocred

echo [ERR] new container not ready (healthz code=!CODE!), last 30 log lines:
call :rollback
exit /b 1

:healthy
echo Done: container %NAME% now runs %IMAGE%
echo   healthz:
curl -s -m 3 http://localhost:%PORT%/healthz
echo.
echo   panel: http://localhost:%PORT%/panel/
exit /b 0

:healthy_nocred
echo Done: container %NAME% now runs %IMAGE% (service up, but account pool is empty)
echo   open the panel to add/check accounts: http://localhost:%PORT%/panel/
exit /b 0

rem ---------- subroutines ----------

:start
rem %1 = image name
docker run -d --name %NAME% -p %PORT%:%PORT% -e TZ=Asia/Shanghai ^
  -v "%ROOT%\auths:/app/auths" ^
  -v "%ROOT%\data:/app/data" ^
  -v "%ROOT%\config.json:/app/config.json" ^
  --restart unless-stopped %~1 >nul
exit /b %errorlevel%

:rollback
docker logs --tail 30 %NAME% 2>&1
docker rm -f %NAME% >nul 2>&1
if not "!OLD_IMAGE!"=="" (
  echo rolling back to previous image !OLD_IMAGE! ...
  call :start !OLD_IMAGE!
  echo rolled back. inspect the new image with: docker run --rm -it !IMAGE! sh
) else (
  echo no previous container to roll back to.
)
exit /b 1
