@echo off
rem Opens the kiosk full screen in Edge as soon as the agent answers.
rem Put a copy in the Startup folder of the account that logs in on the machine:
rem   Win+R, type shell:startup, Enter, and paste this file there.
title Vending kiosk

:wait
curl.exe -s -o NUL --max-time 2 http://127.0.0.1:8080/
if errorlevel 1 (
  timeout /t 2 /nobreak >NUL
  goto wait
)

start "" msedge.exe --kiosk http://127.0.0.1:8080/ --edge-kiosk-type=fullscreen --no-first-run
