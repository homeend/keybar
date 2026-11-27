# Plan: let Windows read the Keydial (Windows as host)

Status (2026-11-27): **Windows reads the Keydial** (`device_windows.go`): buttons
via a low-level keyboard hook that blocks the Keydial's Ctrl+Alt+Shift/Win+F-key
combinations (the Keydial is remapped with `tools/keydialcfg`), the dial via the
wheel hook + Raw Input. **The host follows the Keydial on every OS** (step 3): a
machine becomes the host when the Keydial connects to it
(`Router.KeydialConnected`) and stays the host while the Keydial sleeps, until
another machine says the Keydial is connected there (the `target` message carries
`held`). Then it gives up its target, and takes links from the new host. If both
have it for a moment, the one it leaves last stays the host; if neither has it,
the higher machine ID stays. No machine is the host after a start until the
Keydial connects. The panel reloads when the host changes, and the Learn dialog
shows a small Keydial with the button to press.
Still open: Windows battery level; a "Set up Keydial" button in the panel instead
of `keydialcfg`; holding a button (the Keydial repeats the combo).
The sections below are the original plan and findings.

## Where things stand

- **Linux is the only host.** It reads the Keydial with evdev (`evdev.go`, exclusive
  grab with `EVIOCGRAB`), works out which slot was pressed using the *links* in
  `config.json` (`slots[].trigger`, e.g. `KEY_LEFTSHIFT` → `k13`), and either runs
  the slot itself or sends `down`/`up` for the slot ID to the active paired machine
  (`router.go`).
- **Windows is a receiver.** It runs slots with its own layouts and injects input with
  `SendInput` (`emitter_windows.go`, which maps Linux key names → virtual-key codes
  through `vkOf`). `device_windows.go` is a stub: `DeviceManager` does nothing,
  `batteryPercent` returns -1, `captureShortcut` returns "".
- `hostSupported` is a constant: true in `platform_linux.go`, false in
  `platform_windows.go`. `Router.host` (in `router.go`) is
  `hostSupported && KEYBAR_FAKE_OS == ""`. The host owns the button links and sends
  them to paired machines (`links` message). Receivers store them, show unlinked keys
  as *Not linked*, and forward Learn to the host (`learn` / `learned` messages).
- Networking: Noise XX pairing with a 6-digit code, then Noise KK sessions over TCP
  47831, with UDP multicast discovery on 47832 (`link.go`, `discovery.go`). The
  Windows overlay and machine picker already exist (`overlay_windows.go`).

Moving the Keydial to Windows (plugging its USB receiver or cable in there, or
pairing it over Bluetooth) only helps if Windows can read it. It is connected to one
computer at a time. Presses meant for another machine always need the network.

## What has to be built

### 1. Read the Keydial on Windows (the hard part)

On Windows the KD20 shows up as an ordinary HID keyboard plus mouse. Keybar must
(a) tell its input apart from the real keyboard and mouse, and (b) stop it reaching
other apps. On Linux, `EVIOCGRAB` does both.

| Approach | How | Pros | Cons |
|---|---|---|---|
| **C. HID "vendor mode"** (try first) | Huion's own driver sends the device a setup command; after that, buttons arrive as raw device reports on a vendor HID collection instead of keystrokes. Read them with `hid.dll` / `CreateFile` on the device path (or hidapi). | Nothing leaks to other apps; the device is identified for sure; no driver install; may also give battery. The same code could later replace evdev on Linux. | The KD20 setup command and report format must be worked out. It conflicts with Huion's own app if that is running. |
| **A. Raw Input + low-level keyboard hook** (fallback) | `RegisterRawInputDevices` with `RIDEV_INPUTSINK` reports which device (`hDevice`) each key came from; a `WH_KEYBOARD_LL` hook blocks keys that Raw Input attributed to the Keydial. This is the HidMacros / LuaMacros technique. | Plain Win32 API, no admin rights or driver. | Matching the hook to Raw Input is timing-based and can misfire. The dial (mouse wheel) is harder to block per device: it needs a `WH_MOUSE_LL` hook plus the same matching. |
| B. Interception driver (oblitum/Interception) | A kernel filter driver that sees and blocks input per device | Reliable | Admin install plus reboot; non-commercial licence; third-party kernel driver. Avoid unless A and C fail. |

**Findings on Windows (2026-11-27, Keydial over Bluetooth LE, factory keys):**
- Windows shows it as "Keydial mini-<n>": HID over GATT (`VID&02256C_PID&8251`) with
  four HID collections:

  | Collection | Sends | Can an app open it? |
  |---|---|---|
  | Keyboard `0x01/0x06` | the buttons (bottom row: `Space`, `N`, `Enter`) | No, Windows owns it (access denied) |
  | Mouse `0x01/0x02` | the dial: right = wheel down, left = wheel up (±120 per click) | No, Windows owns it |
  | Consumer `0x0C/0x01` | the dial press: Play/Pause (`02 cd 00`, release `02 00 00`) | Yes, `hid.dll` reads it |
  | Multi-axis `0x01/0x0E` (Surface Dial style, 3-byte feature report) | nothing in factory mode | Yes |

  The GATT side also has the battery service (`0x180F`) and two vendor services
  (`0xFFE0`, `00010203-0405-0607-0809-0a0b0c0d1912`), likely Huion's config / OTA.
- Raw Input (`RIDEV_INPUTSINK`) tags every key and wheel step with the Keydial's device
  handle (its name contains `vid&02256c`), so telling it apart from the real
  keyboard and mouse works with plain Win32. Exception: the Play/Pause key's Raw Input
  event is not attributed to the Keydial; take it from the consumer collection.
- Without blocking, its keys reach the focused app.
- Blocking test (`keydialprobe block`: low-level hooks hold each hardware event until
  Raw Input names its device):
  - **Dial: works.** The wheel's Raw Input event arrives ~0–1.5 ms after the
    `WH_MOUSE_LL` hook even while the hook holds it, so Keydial wheel steps are
    swallowed and other wheels re-sent.
  - **Keys: don't work.** A key held by `WH_KEYBOARD_LL` never gets a Raw Input event
    (from any keyboard): Windows makes it only after the hook lets the key through.
    So approach A can't block the Keydial's keys; every key just waits out the hold.
  - **Remapping works and is stored on the device:** Huion's phone app ("Huion
    Keydial", iOS/Android) offers F1–F12 with modifiers. A button set there to
    Ctrl+Alt+Shift+F12 sends exactly that on Windows with the app closed: Ctrl, Shift,
    Alt, F12 down, then all up, within the same millisecond. Plan: map every button to
    Ctrl+Alt+Shift+F<n> (plus a second modifier set if there are more than 12),
    block those F-keys in `WH_KEYBOARD_LL` when the three modifiers are down, let the
    lone modifiers through and send a mask key (as AutoHotkey's vkE8) so Alt doesn't
    open menus. To check: a held button seems to repeat the whole combination.
  - **Huion's configuration protocol** (from the Huion Keydial app 3.0.4, decompiled):
    one GATT characteristic, `FFE2` in service `FFE0`: commands are written to it,
    replies come as notifications on it (the app requests the max MTU first).
    - Read mapping: `16 02 <group>` (K20: group 0). Reply `16 02 <group>` + 6 bytes per
      button + checksum.
    - Write mapping: `16 01 <group>` + 6 bytes per button + checksum (sum of the
      button bytes mod 256). Reply `16 01` = OK. The app always reads all, changes,
      writes all back.
    - Button = `[type, modifiers, k1, k2, k3, k4]`. Type 1: key combo, modifiers are
      the USB bits (Ctrl 1, Shift 2, Alt 4, Win 8; right-hand ×16), keys are USB HID
      usage codes (F12 = `0x45`). Type 2: media, byte 1 = consumer usage low byte
      (Play `0xCD`). Types 3–9: mouse left/right/middle/forward/back/roll up/roll down.
      The app offers F1–F12 only; F13–F24 (`0x68`–`0x73`) are worth trying.
    - Battery: `CD D1 00 00 00 00 00 00` → `06 D1 <a> <b> …` (percent = b, or a if b
      is 0). Charging: `CD E0 …` → `06 E0 <0/1>`. Sleep: `CD DA <minutes> …`.
      Firmware version: `CD C9 00 00 00 00 00 ED`. Factory reset: `CD FF …`.
    - `00010203-…-1912` is the firmware-update (OTA) service: never write to it.
    - Confirmed from the app's RxBle log (Android, MTU 176 like on Windows): the app
      sends `CD C9…ED`, `CD FE…ED`, then **writes** the mapping (`16 01 00 …`, reply
      `16 01 00 01 00 00`) and only then reads it (`16 02 00`, reply = same block as
      `16 02 00 …`). From Windows a read without a write first got no reply.
    - K20 mapping = 21 entries: 18 buttons, then dial press (factory: media
      Play/Pause), dial one way (mouse roll down `09`), the other way (roll up `08`).
      So the dial turns can be set to key combinations too.
    - From Windows (MediaTek adapter): writing works (`16 01 00 01 00 00` = OK), but the
      130-byte reply to `16 02 00` never arrives (short replies do). So Keybar should
      write a complete mapping of its own rather than read-modify-write on Windows.
    - The Keydial keeps more than one pairing: Linux kept reconnecting after it was
      paired with the phone and Windows. Whichever paired host connects first gets it.
    - **Applied from Windows and verified (2026-11-27):** `keydialcfg <mac> keybar` wrote
      buttons 1–12 = Ctrl+Alt+Shift+F1…F12, 13–18 = Ctrl+Alt+Win+F1…F6, dial press =
      Ctrl+Alt+Win+F7, dial turns = mouse scroll; `keydialprobe watch` showed the new
      combinations coming from the Keydial. Undo: Huion app, or factory reset `CD FF`.
      Previous mapping (captured): see `tools/keydialcfg/main_test.go`.
    - Tool: `tools/keydialcfg` (`go run . <mac> read` / `write "16 01 00 … <sum>"`).
  - Other options for the keys: set the buttons (Huion's Keydial app, if the device stores
    it) to keys no real keyboard sends (F13–F24) and block those unconditionally;
    vendor mode (C); a `WH_KEYBOARD` DLL hook in every app (HidMacros' way, gets
    `WM_INPUT` first); Interception (B).
- Probe code: `tools/keydialprobe` (Windows only; `go run ./tools/keydialprobe caps|watch|block`).

**Step 1, a spike (can be done on Linux, where the device is now):** find out
whether vendor mode works.
- List the KD20's HID interfaces and report descriptors: `/sys/class/hidraw/*/device/report_descriptor`,
  or `usbhid-dump`, or `hid-tools`' `hid-decode`.
- Look for a vendor-defined usage page (0xFF00 and up).
- Check how other open-source Huion drivers switch devices into raw mode. OpenTabletDriver
  reads string descriptors and sends feature/output reports to "init" Huion devices;
  check whether its configurations include any Keydial, and the DIGImend / `hid-uclogic`
  kernel driver code.
- If needed, capture Huion's official driver initialising the device with `usbmon` +
  Wireshark (Linux) or USBPcap (Windows).

The result decides between C and A.

### 2. Use Linux key names on Windows

Make Windows report presses with the same signal names as evdev (`KEY_LEFTSHIFT`,
`REL_WHEEL:up`, …) by inverting `vkOf` (and adding a scan-code path where VK codes
are ambiguous). Links then mean the same thing on every machine, so a link set on
Linux is also correct on Windows. With approach C, map the vendor report bits to slot
IDs or to these names.

### 3. The host is whichever machine has the Keydial (decided at runtime)

Done (see the status at the top). Links travel with the host: a receiver stores the
host's links, so a machine that becomes the host starts from the last set it got.
This works because both OSes report the remapped Keydial with the same signatures.

- Replace the `hostSupported` constant with "this machine has the Keydial connected
  now" (DeviceManager status). `Router.host` becomes dynamic.
- When the host changes, the new host announces itself (`target` message) and sends
  its links. Receivers already accept links only when they aren't the host.
- Links: either each host keeps its own links (simple), or links travel with the
  config so the new host starts from the last known set (nicer, possible after
  step 2). Recommended: when a machine becomes host and has no links, take the
  last ones it received.
- Two Keydials at once (one per machine): each host routes its own. Make sure
  `seen` / `RemoteLearn` pick the right host (today it assumes one).

### 4. Windows panel and tray

- `server.go` injects `__HOST__` into the page. It must follow the runtime host flag,
  not the constant.
- Learn, Pause, device status and battery in `/api/status`.
- The tray already hides Pause when `!hostSupported`; make that runtime too.
- `captureShortcut` on Windows (used by "Record shortcut" in the panel) could use the
  same hook.

### 5. Linux as a receiver

Already works: `Router.PeerMsg` handles `down` / `up` on any machine, and Linux
injects with uinput.

## Logitech Flow-like tools (one pointer across computers)

These share **all** input between computers when the pointer crosses a screen edge.
They capture with low-level hooks and inject with `SendInput` on Windows, like
Keybar's receiving side. They don't single out one device, which is what Keybar adds.

| Project | Notes |
|---|---|
| Deskflow | Open-source upstream of Synergy (since 2024), active. Windows, macOS, Linux, including Wayland through libei / the input-capture portal. |
| Input Leap | Community fork of Barrier. Cross-platform. |
| lan-mouse | Rust, Wayland-first, also Windows and macOS. Encrypted, small codebase, good to read. |
| Mouse Without Borders | Microsoft, part of PowerToys (MIT). Windows only, closest to Logitech Flow (clipboard, files). |
| Barrier | Unmaintained; use Input Leap. |

Options:
- Run one of them next to Keybar (pointer and keyboard), with Keybar for the Keydial.
  Optionally make Keybar's target follow the tool's active screen (Deskflow logs or IPC).
- Pointer sharing inside Keybar is feasible (pairing, encryption and routing exist),
  but it's a large feature with many edge cases (DPI, multiple monitors, Wayland,
  admin windows).

## Suggested order

1. Vendor-mode spike (small). Decides C or A.
2. Windows reads the Keydial (medium-large).
3. Runtime host and links follow the device (medium).
4. Panel and tray on Windows (small-medium).
5. Optional: follow Deskflow's / lan-mouse's active screen.

## Building and testing on Windows

- `build-windows.cmd` (runs `go build -ldflags "-H=windowsgui" -o keybar.exe .`).
- Logs: `collect-logs.cmd` copies `keybar.log` and `keybar.prev.log` into `logs/`
  (gitignored).
- Tests: `go test -race ./...`. Most networking tests run on any OS.
- A second copy can run on one machine for testing: set `KEYBAR_HOME`,
  `KEYBAR_DRY_RUN=1`, `KEYBAR_NO_TRAY=1`, `KEYBAR_FAKE_OS` (see `make test-peer`).
