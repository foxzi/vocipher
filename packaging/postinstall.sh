#!/bin/sh
set -e

# Create user and group
if ! getent group vocipher >/dev/null 2>&1; then
    groupadd --system vocipher
fi
if ! getent passwd vocipher >/dev/null 2>&1; then
    useradd --system --gid vocipher --home-dir /var/lib/vocipher --shell /usr/sbin/nologin vocipher
fi

# Create data directory
mkdir -p /var/lib/vocipher
chown vocipher:vocipher /var/lib/vocipher
chmod 750 /var/lib/vocipher

# Install default config if not present
if [ ! -f /etc/vocipher/config.yaml ]; then
    cp /etc/vocipher/config.yaml.default /etc/vocipher/config.yaml
    chown root:vocipher /etc/vocipher/config.yaml
    chmod 640 /etc/vocipher/config.yaml
fi

# Enable service
systemctl daemon-reload
systemctl enable vocipher.service

# Restart if already running (upgrade), otherwise just print instructions
if systemctl is-active --quiet vocipher.service 2>/dev/null || systemctl is-enabled --quiet vocipher.service 2>/dev/null; then
    systemctl restart vocipher.service
    echo "Vocipher updated and restarted."
else
    echo "Vocipher installed. Edit /etc/vocipher/config.yaml then run:"
    echo "  systemctl start vocipher"
fi
