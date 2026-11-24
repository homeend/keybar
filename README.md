# Keybar

Maps the buttons and dial of a **Huion Keydial mini (KD20)** to shortcuts, apps,
commands, mouse events and typed text on Linux (GNOME/Wayland). Single Go binary:
a tray icon, a daemon that reads the device, and a control panel at
<http://127.0.0.1:47830>.

## How it works
- The Keydial sends factory shortcuts (e.g. `Ctrl+Z`, `B`, wheel). Keybar opens its
  `/dev/input/event*` nodes (USB id `256c:8251`), turns each into a *trigger*
  (`KEY_LEFTCTRL+KEY_Z`, `REL_WHEEL:up`…), and runs the action bound to it.
- With **take over** on (default) the device is grabbed (`EVIOCGRAB`) so its factory
  keys don't leak; unassigned buttons are re-sent unchanged. Keys and clicks are
  sent through two virtual uinput devices.
- Reconnects (Bluetooth sleep) are picked up automatically every 2 s.

## Setup
```bash
sudo ./install-udev.sh   # once: lets the input group use /dev/uinput
make install             # build, install to ~/.local/bin, enable systemd user service
```
Open the panel from the app grid ("Keybar"), the tray icon, or `keybar open`.

## Using the panel
1. Click a key or part of the dial → **Learn** → press that button on the device
   (or **Learn all buttons** to go through every slot).
2. Pick what it does: Shortcut (record from your keyboard or use a preset), App,
   Command (voxtype voice presets included, optional release command for
   push-to-talk), Mouse, Text, Factory key, Nothing. Changes save instantly.

## Files
- Config: `~/.config/keybar/config.json`; log: `~/.cache/keybar/keybar.log`
- `make restart`, `make logs`, `make uninstall`; `keybar devices` lists input devices.
