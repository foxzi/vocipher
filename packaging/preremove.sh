#!/bin/sh
set -e

# Only stop and disable on full removal, not upgrade
if [ "$1" = "remove" ] || [ "$1" = "0" ]; then
    systemctl stop vocala.service 2>/dev/null || true
    systemctl disable vocala.service 2>/dev/null || true
fi
