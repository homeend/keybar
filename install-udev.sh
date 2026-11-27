#!/usr/bin/env bash
# One-time (root) setup: let members of the "input" group create virtual
# keyboards/mice via /dev/uinput, which Keybar uses to send keys and clicks.
set -euo pipefail
if [[ $EUID -ne 0 ]]; then exec sudo "$0" "$@"; fi
cat > /etc/udev/rules.d/60-keybar-uinput.rules <<'RULE'
# Keybar: allow the input group to use uinput (virtual keyboard/mouse)
KERNEL=="uinput", SUBSYSTEM=="misc", GROUP="input", MODE="0660", OPTIONS+="static_node=uinput"
RULE
# The desktop must not use the Keydial itself: Keybar reads it and sends the
# keys. Otherwise a press that wakes the Keydial reaches the desktop before
# Keybar takes the device over, and its release never does (stuck key).
cat > /etc/udev/rules.d/70-keybar-keydial.rules <<'RULE'
# Keybar: the desktop ignores the Huion Keydial; Keybar reads it and sends keys itself.
# Without Keybar running, the Keydial does nothing. Remove this file to undo.
SUBSYSTEM=="input", KERNEL=="event*", ATTRS{id/vendor}=="256c", ATTRS{name}=="Keydial*", ENV{LIBINPUT_IGNORE_DEVICE}="1", GROUP="input", MODE="0660"
RULE
echo uinput > /etc/modules-load.d/keybar-uinput.conf
modprobe uinput || true
udevadm control --reload-rules
udevadm trigger --subsystem-match=misc --sysname-match=uinput || true
udevadm trigger --subsystem-match=input --attr-match=id/vendor=256c || true
chgrp input /dev/uinput && chmod 0660 /dev/uinput
ls -l /dev/uinput
echo "Done. Keybar will pick this up within a few seconds."
echo "Turn the Keydial off and on (or let it sleep and wake it) so the desktop lets go of it."
