# Keybar

Maps the buttons and dial of a **Huion Keydial mini (KD20)** to shortcuts, apps,
commands, mouse events and typed text on Linux (GNOME/Wayland), and can drive
other computers (Linux or Windows) over the network. Single Go binary:
a tray icon, a daemon that reads the device, and a control panel at
<http://127.0.0.1:47830>.

## How it works
- The Keydial sends factory shortcuts (e.g. `Ctrl+Z`, `B`, wheel). Keybar opens its
  `/dev/input/event*` nodes (USB id `256c:8251`), turns each into a *trigger*
  (`KEY_LEFTCTRL+KEY_Z`, `REL_WHEEL:up`…), and runs the action bound to it.
- With **take over** on (default) the device is grabbed (`EVIOCGRAB`) so its factory
  keys don't leak; unassigned buttons are re-sent unchanged. Keys and clicks are
  sent through two virtual uinput devices.
- Reconnects (Bluetooth sleep) are picked up as soon as the device appears. If the
  button that woke the Keydial is still held then, Keybar waits for its release
  before taking the device over, so the desktop never sees a key stuck down.

## Setup
```bash
sudo ./install-udev.sh   # once: lets the input group use /dev/uinput, and makes
                         # the desktop ignore the Keydial (Keybar reads it instead)
make install             # build, install to ~/.local/bin, enable systemd user service
```
With the udev rule the desktop never receives the Keydial's own keys, so a press
can't leak before Keybar takes over. The flip side: without Keybar running the
Keydial does nothing. **Pause mappings** (tray) makes Keybar pass the factory keys
through unchanged. To undo, delete `/etc/udev/rules.d/70-keybar-keydial.rules`.

Open the panel from the app grid ("Keybar"), the tray icon, or `keybar open`.

## Using the panel
1. Click a key or part of the dial → **Learn** → press that button on the device
   (or **Learn all buttons** to go through every slot).
2. Pick what it does: Shortcut (record from your keyboard or use a preset), App,
   Command (voxtype voice presets included, optional release command for
   push-to-talk), Mouse, Text, Factory key, Nothing. Changes save instantly.

## Layouts
Each machine can have several layouts (tabs above the device). One is active; a key
with the **Layout** action switches to the next/previous/a named one. Give every
layout a way back.

## Several computers
One Keydial can control several computers on the same network. The computer it is
plugged into (Linux, for now) reads it; Keybar on the other computers receives the
presses and runs them with **their own** active layout.

1. Run Keybar on both computers. They find each other by themselves
   (🖧 **Machines** → *Nearby*).
2. Click **Pair…**, check both screens show the same 6-digit code, and confirm on
   both. Keybar then offers to copy layouts; shortcuts are converted for the other
   OS where needed (Super ↔ Win gestures, workspaces ↔ virtual desktops, screenshot),
   apps are matched by name, and what can't be converted is marked ⚠.
3. Give a key the **Machine** action. Press it to open the machine picker, turn the
   dial to choose, then press it again (or the dial) to switch. Holding it, turning
   and releasing does the same in one go. Any other key, or 10 seconds, closes the
   picker without switching.

Which physical button is which key (its *link*) is stored only on the computer the
Keydial is plugged into, and sent to the others. A key that isn't linked there shows
*Not linked* on every machine and can't be given an action until it is: select it
and press the button. This works from any machine's panel while the Keydial's
computer is connected. To bring a layout over again, use **Copy from…** → *Into:
Replace the keys of …*.

Security: discovery beacons carry no secrets. Pairing uses the Noise protocol (XX)
with a 6-digit comparison code; afterwards every connection uses Noise KK, so only
paired machines can connect and all traffic is encrypted. Keys live in
`identity.json` / `peers.json` (owner-only). Ports: UDP 47832 (discovery),
TCP 47831 (paired machines).

## Windows (receiver)
Needs [Go](https://go.dev/dl/) and Git.
```bat
git clone <this repo> keybar && cd keybar
build-windows.cmd        :: builds keybar.exe without a console window
keybar.exe               :: tray icon + panel at http://127.0.0.1:47830
keybar.exe install       :: start at sign-in (uninstall: keybar.exe uninstall)
```
- When Windows asks, **allow Keybar on private networks**. The network must be set to
  *Private* (Settings → Network → your connection) — on *Public* networks Windows
  blocks discovery and incoming connections. If the first prompt was dismissed:
  `netsh advfirewall firewall add rule name=Keybar dir=in action=allow program="%CD%\keybar.exe" enable=yes profile=private` (admin).
- Apps are the Start-menu shortcuts; commands run with `cmd /C`.
- Windows doesn't let a normal program type into apps running as administrator.
- Config: `%APPDATA%\keybar`; log: `%LOCALAPPDATA%\keybar\keybar.log`.

## Testing without a second computer
`make test-peer` starts a second Keybar on this computer that pretends to be a
Windows machine (panel http://127.0.0.1:47840, actions only logged).

## Logs
`collect-logs.cmd` (Windows) / `./collect-logs.sh` (Linux) copy the current and the
previous run's log into `logs/` in this folder (git-ignored), named after the computer.

## Files
- Config: `~/.config/keybar/config.json`; log: `~/.cache/keybar/keybar.log`
- `make restart`, `make logs`, `make uninstall`; `keybar devices` lists input devices.
