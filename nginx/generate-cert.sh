#!/bin/sh
# Generate self-signed certificate for local/testing HTTPS
set -e

CERT_DIR="${1:-./certs}"
mkdir -p "$CERT_DIR"

if [ -f "$CERT_DIR/selfsigned.crt" ] && [ -f "$CERT_DIR/selfsigned.key" ]; then
    echo "Certificates already exist in $CERT_DIR, skipping generation"
    exit 0
fi

# Detect local IP for SAN
LOCAL_IP=$(hostname -I | awk '{print $1}')

openssl req -x509 -nodes -days 365 \
    -newkey rsa:2048 \
    -keyout "$CERT_DIR/selfsigned.key" \
    -out "$CERT_DIR/selfsigned.crt" \
    -subj "/CN=vocala/O=Vocala/C=US" \
    -addext "subjectAltName=DNS:localhost,IP:127.0.0.1,IP:${LOCAL_IP}"

echo "Self-signed certificate generated in $CERT_DIR (SAN includes ${LOCAL_IP})"
