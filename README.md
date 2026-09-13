# Hexagon (Nefarious Hub)

Turn one Android device into a Roblox multi-instance control center from Termux.

Hexagon is an open-source launcher and watchdog that helps you start, monitor, and recover multiple Roblox client clones with less manual tapping.

## Why this helps for Roblox

- **Launch multiple clones faster** instead of opening every app manually.
- **Stay in-game longer** with automatic reconnect/recovery behavior.
- **Handle disconnects and freezes** with built-in 24/7 monitoring logic.
- **Target the game you want** using a Place ID, game URL, or private server link.
- **Track clone health live** with per-clone status, PID, and memory telemetry.
- **Get Discord alerts** when something crashes, restarts, or reconnects.

## What it does

- Interactive setup flow in terminal UI
- Clone count recommendation based on RAM/CPU
- Auto-launch sequencing and cooldown handling
- Crash/freeze/disconnect detection with automated relaunch
- Network outage/IP-change recovery behavior
- Optional Discord webhook notifications
- Version check and updater launcher script

## Quick start (Termux)

```bash
pkg update -y && pkg upgrade -y
pkg install -y curl
curl -fsSL https://raw.githubusercontent.com/relayced/Hexagon/main/nefhub.sh -o nefhub.sh
chmod +x nefhub.sh
./nefhub.sh
```

The launcher will detect your architecture, download the correct binary, and start the hub.

## Typical setup flow

1. Pick how many Roblox clones you want to run.
2. Enter your target experience (game URL, Place ID, or private server link).
3. Enable Sentinel monitoring/auto-rejoin.
4. (Optional) Connect Discord webhook for alerts.
5. Launch and keep Termux running while monitoring is active.

## Requirements

- Android device with **Termux**
- Roblox clone package setup compatible with your device
- Stable internet connection for long sessions
- Enough RAM/CPU for your selected clone count

## Important notes

- Use responsibly and follow Roblox rules, game rules, and local policies.
- Higher clone counts increase thermal and battery load.
- This project is designed to reduce repetitive manual recovery/launch work, not guarantee uninterrupted uptime.

## Project files

- `main.go` – core hub runtime and monitoring engine
- `nefhub.sh` – architecture-aware launcher/updater
- `version.txt` – latest required version marker
- `CHANGELOGS.TXT` – feature and patch history
