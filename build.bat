@echo off
set GOOS=windows
set GOARCH=amd64
go build -tags "desktop production" -ldflags="-s -w -H windowsgui" -o build\bin\vintecinco_dl.exe .
if %errorlevel%==0 (
    echo Build successful: build\bin\vintecinco_dl.exe
) else (
    echo Build failed.
)
