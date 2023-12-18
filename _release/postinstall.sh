#!/bin/bash

# Check that dnsutils is installed
if ! command -v nslookup &> /dev/null; then
  echo "nslookup not found, can be installed with: sudo apt install dnsutils"
  exit 1
fi

udevadm control --reload-rules

systemctl daemon-reload
systemctl enable rpi-net-manager.service
systemctl restart rpi-net-manager.service
