@echo off
setlocal enabledelayedexpansion

:: Navigate to repository root directory
cd /d "%~dp0"

set RUN_TESTS=0
set CLEAN=0
set RELEASE=0

:parse_args
if "%~1"=="" goto end_parse
if /i "%~1"=="-t" (
    set RUN_TESTS=1
    shift
    goto parse_args
)
if /i "%~1"=="--test" (
    set RUN_TESTS=1
    shift
    goto parse_args
)
if /i "%~1"=="-r" (
    set RELEASE=1
    shift
    goto parse_args
)
if /i "%~1"=="--release" (
    set RELEASE=1
    shift
    goto parse_args
)
if /i "%~1"=="-c" (
    set CLEAN=1
    shift
    goto parse_args
)
if /i "%~1"=="--clean" (
    set CLEAN=1
    shift
    goto parse_args
)
if /i "%~1"=="-h" (
    goto usage
)
if /i "%~1"=="--help" (
    goto usage
)
if /i "%~1"=="/?" (
    goto usage
)

echo Unknown option: %~1
:usage_error
call :show_usage
exit /b 1

:usage
call :show_usage
exit /b 0

:show_usage
echo Usage: build.bat [OPTIONS]
echo.
echo Build BabelGate / LLM-Router locally on Windows
echo.
echo Options:
echo   -t, --test       Run unit tests before building
echo   -r, --release    Build optimized release binary (stripped symbols)
echo   -c, --clean      Clean previous build artifacts before building
echo   -h, --help       Show this help message
goto :eof

:end_parse

if "%CLEAN%"=="1" (
    echo ==^> Cleaning bin\ directory...
    if exist bin (
        rmdir /s /q bin
    )
)

if "%RUN_TESTS%"=="1" (
    echo ==^> Running unit tests...
    go test ./...
    if errorlevel 1 (
        echo Error: Unit tests failed.
        exit /b %errorlevel%
    )
    echo [OK] All tests passed!
)

echo ==^> Building babelgate...
if not exist bin mkdir bin

if "%RELEASE%"=="1" (
    go build -trimpath -ldflags="-s -w" -o bin\babelgate.exe .\cmd\router
) else (
    go build -o bin\babelgate.exe .\cmd\router
)

if errorlevel 1 (
    echo Error: Build failed.
    exit /b %errorlevel%
)

set "BIN_PATH=bin\babelgate.exe"
if exist "%BIN_PATH%" (
    for %%A in ("%BIN_PATH%") do (
        set "BYTES=%%~zA"
        set /a "MB=!BYTES! / 1048576"
        echo [OK] Successfully built %BIN_PATH% ^(!MB! MB / !BYTES! bytes^)
    )
)

echo.
echo Run with:
echo   .\bin\babelgate.exe
exit /b 0
