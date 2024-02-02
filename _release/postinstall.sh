#!/bin/bash

# TODO Install dnsmasq

systemctl daemon-reload
systemctl enable rpi-net-manager.service
systemctl restart rpi-net-manager.service
