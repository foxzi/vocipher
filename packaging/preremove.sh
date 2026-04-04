#!/bin/sh
set -e

# Only stop and disable on full removal, not upgrade
if [ "$1" = "remove" ] || [ "$1" = "0" ]; then
    systemctl stop vocipher.service 2>/dev/null || true
    systemctl disable vocipher.service 2>/dev/null || true
fi
