#!/bin/bash
# MiMo key scraper cron wrapper
# Runs the scraper and posts keys to the local API.
# Prerequisites: Chrome with --remote-debugging-port=9222, Go server running on :3000

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

node scrape_mimo_keys.js --post-to-api >> /tmp/mimo-scraper.log 2>&1
