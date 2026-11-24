#!/usr/bin/env bash
# One-time (root) setup: let members of the "input" group create virtual
# keyboards/mice via /dev/uinput, which Keybar uses to send keys and clicks.
set -euo pipefail
if [[ $EUID -ne 0 ]]; then exec sudo "$0" "$@"; fi
cat > /etc/udev/rules.d/60-keybar-uinput.rules <<'RULE'
# Keybar: allow the input group to use uinput (virtual keyboard/mouse)
KERNEL=="uinput", SUBSYSTEM=="misc", GROUP="input", MODE="0660", OPTIONS+="static_node=uinput"
RULE
echo uinput > /etc/modules-load.d/keybar-uinput.conf
modprobe uinput || true
udevadm control --reload-rules
udevadm trigger --subsystem-match=misc --sysname-match=uinput || true
chgrp input /dev/uinput && chmod 0660 /dev/uinput
ls -l /dev/uinput
echo "Done. Keybar will pick this up within a few seconds."
