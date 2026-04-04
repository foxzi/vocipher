#!/bin/sh
set -e

# Create user and group
if ! getent group vocala >/dev/null 2>&1; then
    groupadd --system vocala
fi
if ! getent passwd vocala >/dev/null 2>&1; then
    useradd --system --gid vocala --home-dir /var/lib/vocala --shell /usr/sbin/nologin vocala
fi

# Create data directory
mkdir -p /var/lib/vocala
chown vocala:vocala /var/lib/vocala
chmod 750 /var/lib/vocala

# Install default config if not present
if [ ! -f /etc/vocala/config.yaml ]; then
    cp /etc/vocala/config.yaml.default /etc/vocala/config.yaml
    chown root:vocala /etc/vocala/config.yaml
    chmod 640 /etc/vocala/config.yaml
fi

# Enable service
systemctl daemon-reload
systemctl enable vocala.service

# Restart if already running (upgrade), otherwise just print instructions
if systemctl is-active --quiet vocala.service 2>/dev/null || systemctl is-enabled --quiet vocala.service 2>/dev/null; then
    systemctl restart vocala.service
    echo "Vocala updated and restarted."
else
    echo "Vocala installed. Edit /etc/vocala/config.yaml then run:"
    echo "  systemctl start vocala"
fi
