# Plan: let Windows read the Keydial (Windows as host)

Status: plan only, nothing implemented yet. Written so the work can be picked up on
the Windows machine.

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
