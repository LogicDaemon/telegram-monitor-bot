#!/bin/bash

set -xe

go build
sudo mv telegram-monitor /usr/local/bin/telegram-monitor
envsubst < telegram-monitor.service.template | sudo tee /etc/systemd/system/telegram-monitor.service
sudo systemctl daemon-reload
sudo systemctl enable telegram-monitor.service
sudo systemctl start telegram-monitor.service
