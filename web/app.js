"use strict";
const TOKEN = window.KEYBAR_TOKEN;
// Receiver-only machines (Windows for now) never see the Keydial themselves: presses
// arrive from the paired machine it's plugged into, so every key counts as linked.
const RECEIVER = window.KEYBAR_HOST === false;
const linked = b => RECEIVER || !!b.trigger;
const WIN = window.KEYBAR_OS === "windows";
const $ = (s, r = document) => r.querySelector(s);

let cfg = null, status = null, selected = null, apps = null, netState = null;
let learn = null; // { id, queue: [ids] } — explicit "Learn" dialog
let autoLearn = null; // id of an unlinked slot that links to the next button press

// ---------- helpers ----------
function el(tag, attrs = {}, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v == null || v === false) continue;
    if (k === "class") e.className = v;
    else if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
    else if (k === "value") e.value = v;
    else e.setAttribute(k, v === true ? "" : v);
  }
  for (const k of kids.flat()) if (k != null && k !== false) e.append(k.nodeType ? k : String(k));
  return e;
}
async function api(method, path, body) {
  const r = await fetch(path, {
    method, headers: { "X-Token": TOKEN, "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(data.error || r.statusText);
  return data;
}
let toastTimer;
function toast(msg, ms = 3200) {
  const t = $("#toast"); t.textContent = msg; t.hidden = false;
  clearTimeout(toastTimer); toastTimer = setTimeout(() => (t.hidden = true), ms);
}
// The device has fixed slots (cfg.slots: position + linked button); each layout
// says what every slot does. A "view" merges the two for the layout being edited.
let editLayout = null; // id of the layout shown in the panel (defaults to the active one)
const curLayout = () => cfg.layouts.find(l => l.id === editLayout) || cfg.layouts.find(l => l.id === cfg.active_layout) || cfg.layouts[0];
const layoutName = id => (cfg.layouts.find(l => l.id === id) || {}).name || id;
function view(slot) {
  const key = () => { const k = curLayout().keys; return k[slot.id] || (k[slot.id] = { label: slot.id.toUpperCase(), action: { type: "none" } }); };
  return {
    get id() { return slot.id; }, get group() { return slot.group; },
    get x() { return slot.x; }, get y() { return slot.y; }, get w() { return slot.w; }, get h() { return slot.h; },
    get trigger() { return slot.trigger; }, set trigger(v) { slot.trigger = v; },
    get label() { return key().label; }, set label(v) { key().label = v; },
    get icon() { return key().icon; }, set icon(v) { key().icon = v; },
    get action() { return key().action; }, set action(v) { key().action = v; },
    get note() { return key().note || ""; }, set note(v) { key().note = v || undefined; },
    get info() { return key().info || ""; }, set info(v) { key().info = v || undefined; },
  };
}
const bindings = () => cfg.slots.map(view);
const byId = id => { const s = cfg.slots.find(s => s.id === id); return s && view(s); };

// ---------- pretty names ----------
const MODS = { KEY_LEFTCTRL: "Ctrl", KEY_RIGHTCTRL: "RCtrl", KEY_LEFTSHIFT: "Shift", KEY_RIGHTSHIFT: "RShift",
  KEY_LEFTALT: "Alt", KEY_RIGHTALT: "AltGr", KEY_LEFTMETA: WIN ? "Win" : "Super", KEY_RIGHTMETA: WIN ? "RWin" : "RSuper" };
const NICE = { ESC: "Esc", ENTER: "Enter", SPACE: "Space", TAB: "Tab", BACKSPACE: "Backspace", DELETE: "Del",
  SYSRQ: "PrtSc", PRINT: "PrtSc", VOLUMEUP: "Vol +", VOLUMEDOWN: "Vol −", MUTE: "Mute", MICMUTE: "Mic mute",
  PLAYPAUSE: "Play/Pause", NEXTSONG: "Next", PREVIOUSSONG: "Prev", PAGEUP: "PgUp", PAGEDOWN: "PgDn",
  LEFTBRACE: "[", RIGHTBRACE: "]", MINUS: "-", EQUAL: "=", COMMA: ",", DOT: ".", SLASH: "/", SEMICOLON: ";",
  APOSTROPHE: "'", GRAVE: "`", BACKSLASH: "\\", UP: "↑", DOWN: "↓", LEFT: "←", RIGHT: "→" };
const ALIAS = { ctrl: "KEY_LEFTCTRL", control: "KEY_LEFTCTRL", shift: "KEY_LEFTSHIFT", alt: "KEY_LEFTALT",
  super: "KEY_LEFTMETA", meta: "KEY_LEFTMETA", win: "KEY_LEFTMETA", altgr: "KEY_RIGHTALT", esc: "KEY_ESC",
  del: "KEY_DELETE", print: "KEY_SYSRQ", prtsc: "KEY_SYSRQ", volup: "KEY_VOLUMEUP", voldown: "KEY_VOLUMEDOWN", play: "KEY_PLAYPAUSE" };

function prettyKey(name) {
  if (MODS[name]) return MODS[name];
  const n = name.replace(/^(KEY|BTN)_/, "");
  if (NICE[n]) return NICE[n];
  return n.length <= 1 ? n : n[0] + n.slice(1).toLowerCase();
}
function prettyTrigger(sig) {
  if (!sig) return "";
  return sig.split("+").map(p => {
    const [name, dir] = p.split(":");
    if (!name.startsWith("REL_")) return prettyKey(name);
    const up = dir === "up";
    if (name === "REL_WHEEL") return "Wheel " + (up ? "↑" : "↓");
    if (name === "REL_HWHEEL") return "Wheel " + (up ? "→" : "←");
    return "Dial " + (up ? "↻" : "↺");
  }).join(" + ");
}
function prettyShortcut(s) {
  return (s || "").split(/[,\s]+/).filter(Boolean).map(c => c.split("+").filter(Boolean).map(k => {
    const a = ALIAS[k.toLowerCase()];
    return prettyKey(a || "KEY_" + k.toUpperCase());
  }).join("+")).join(", ");
}
const MOUSE = { left: "Left click", right: "Right click", middle: "Middle click", back: "Back button",
  forward: "Forward button", scroll_up: "Scroll up", scroll_down: "Scroll down", scroll_left: "Scroll left",
  scroll_right: "Scroll right", move: "Move pointer" };

function summary(b) {
  const a = b.action || { type: "none" };
  switch (a.type) {
    case "keys": return a.keys ? "⌨ " + prettyShortcut(a.keys) : "⌨ (empty)";
    case "app": return a.app_name || a.app || "App";
    case "command": return "▸ " + (a.command || "(empty)");
    case "mouse": return "🖱 " + (MOUSE[a.mouse] || "Mouse");
    case "text": return "“" + (a.text || "") + "”";
    case "layout": return "⇄ " + (a.layout === "prev" ? "Prev layout" : a.layout && a.layout !== "next" ? layoutName(a.layout) : "Next layout");
    case "machine": return "🖧 " + ({ pick: "Pick machine", next: "Next machine", self: "This machine" }[a.machine || "pick"] || machineName(a.machine));
    case "passthrough": return linked(b) ? "Factory key" : "Not linked";
    default: return linked(b) ? "Does nothing" : "Not linked";
  }
}
function appIcon(a) {
  const app = apps && apps.find(x => x.id === a.app);
  const icon = app ? app.icon : a.app_icon;
  return icon ? "/api/icon?name=" + encodeURIComponent(icon) : null;
}

// ---------- device rendering ----------
// Factory marks printed on the KD20 keycaps, by grid position, so the drawing
// matches the device in hand.
const GLYPHS = { "1,1": "▲", "0,2": "◀", "1,2": "–", "2,2": "▶", "1,3": "▼", "3,3": "●", "0,4": "●" };
function renderDevice() {
  const keys = $("#keys");
  keys.style.gridTemplateColumns = `repeat(${cfg.columns || 4}, 1fr)`;
  keys.replaceChildren();
  for (const b of bindings().filter(b => b.group === "keys")) {
    const act = el("div", { class: "act" });
    if (b.action.type === "app") {
      const src = appIcon(b.action);
      if (src) act.append(el("img", { src, alt: "", onerror: e => e.target.remove() }));
    }
    act.append(summary(b));
    const w = b.w || 1, h = b.h || 1;
    const glyph = GLYPHS[`${b.x},${b.y}`];
    const k = el("button", {
      class: "key" + (w > 1 || h > 1 ? " span" : "") + (h > 1 ? " tall" : "") + (w > 1 ? " wide" : "") + (linked(b) ? "" : " unlearned") + (selected === b.id ? " selected" : ""),
      "data-id": b.id, title: RECEIVER ? b.label : b.trigger ? "Device sends: " + prettyTrigger(b.trigger) : "Not linked yet — click to link",
      onclick: () => select(b.id),
    }, el("div", { class: "lbl" }, b.label), act,
      b.icon ? el("span", { class: "emoji" }, b.icon) : b.trigger ? el("span", { class: "phys", title: "Linked button sends " + prettyTrigger(b.trigger) }, prettyTrigger(b.trigger)) : null,
      glyph ? el("span", { class: "glyph" }, glyph) : null,
      b.note ? el("span", { class: "warn", title: b.note }, "⚠") : b.info ? el("span", { class: "warn info", title: b.info }, "ℹ") : null);
    k.style.gridColumn = `${b.x + 1} / span ${b.w || 1}`;
    k.style.gridRow = `${b.y + 1} / span ${b.h || 1}`;
    keys.append(k);
  }
  for (const part of document.querySelectorAll(".dial-part")) {
    const b = byId(part.dataset.id);
    if (!b) continue;
    part.classList.toggle("selected", selected === b.id);
    part.title = b.label + " — " + summary(b);
    part.querySelector("small").textContent = (b.action.type === "passthrough" || b.action.type === "none") && !linked(b) ? "" : summary(b);
    part.onclick = () => select(b.id);
  }
}

function flash(id) {
  const n = document.querySelector(`[data-id="${CSS.escape(id)}"]`);
  if (!n) return;
  n.classList.remove("flash"); void n.offsetWidth; n.classList.add("flash");
  setTimeout(() => n.classList.remove("flash"), 450);
}

// ---------- layouts ----------
function renderAll() { renderLayouts(); renderDevice(); renderEditor(); }
function renderLayouts() {
  const L = curLayout();
  $("#layouts").replaceChildren(
    ...cfg.layouts.map(l => el("button", {
        class: "lt" + (l.id === L.id ? " on" : ""), title: l.id === cfg.active_layout ? "Active layout" : "Edit this layout (not active)",
        onclick: () => { editLayout = l.id; renderAll(); } },
      l.id === cfg.active_layout ? el("span", { class: "live", title: "Active" }, "●") : null, l.name)),
    el("button", { class: "lt add", title: "New layout", onclick: () => addLayout(false) }, "+"));
  $("#layoutTools").replaceChildren(
    L.id === cfg.active_layout
      ? el("span", { class: "muted small" }, el("span", { class: "live" }, "●"), " Active on this machine")
      : el("button", { class: "btn small primary", onclick: () => activateLayout(L.id) }, "Make active"),
    el("div", { style: "flex:1" }),
    el("button", { class: "btn small ghost", title: "Copy layouts from a paired machine", onclick: () => openCopy() }, "Copy from…"),
    el("button", { class: "btn small ghost", onclick: renameLayout }, "Rename"),
    el("button", { class: "btn small ghost", onclick: () => addLayout(true) }, "Duplicate"),
    ...(cfg.layouts.length > 1 ? [el("button", { class: "btn small ghost", onclick: deleteLayout }, "Delete")] : []));
}
function addLayout(copy) {
  const L = curLayout();
  const name = prompt(copy ? "Name of the copy" : "Name of the new layout", copy ? L.name + " copy" : "");
  if (!name || !name.trim()) return;
  const keys = {};
  for (const s of cfg.slots) keys[s.id] = copy ? structuredClone(L.keys[s.id]) : { label: (L.keys[s.id] || {}).label || s.id.toUpperCase(), action: { type: "none" } };
  const l = { id: "l" + Date.now().toString(36), name: name.trim(), keys };
  cfg.layouts.push(l);
  editLayout = l.id; selected = null;
  save(true); renderAll();
}
function renameLayout() {
  const L = curLayout();
  const name = prompt("Rename layout", L.name);
  if (!name || !name.trim()) return;
  L.name = name.trim(); save(true); renderAll();
}
function deleteLayout() {
  const L = curLayout();
  if (!confirm(`Delete layout “${L.name}”? This cannot be undone.`)) return;
  cfg.layouts = cfg.layouts.filter(l => l !== L);
  if (cfg.active_layout === L.id) cfg.active_layout = cfg.layouts[0].id;
  editLayout = cfg.active_layout; selected = null;
  save(true); renderAll();
}
async function activateLayout(id) {
  await save(true);
  try { const c = await api("POST", "/api/layout", { layout: id }); cfg.active_layout = c.active_layout; }
  catch (e) { toast(e.message); }
  renderAll();
}

// ---------- saving ----------
let saveTimer, savePromise;
function save(now = false) {
  clearTimeout(saveTimer);
  const run = async () => {
    try {
      // Keep our own cfg object: the editor's handlers point into it, so replacing
      // it with the server's copy would make the next click edit a stale object.
      await api("PUT", "/api/config", cfg);
      const s = $("#savedMark"); if (s) { s.classList.add("show"); setTimeout(() => s.classList.remove("show"), 1200); }
      const e = $("#edErr"); if (e) e.textContent = "";
    } catch (err) {
      const e = $("#edErr"); if (e) e.textContent = err.message; else toast(err.message);
    }
    renderDevice();
  };
  if (now) return (savePromise = run());
  saveTimer = setTimeout(run, 350);
}

// ---------- editor ----------
const TYPES = [["keys", "Shortcut"], ["app", "App"], ["command", "Command"], ["mouse", "Mouse"], ["text", "Text"], ["layout", "Layout"], ["machine", "Machine"], ["passthrough", "Factory key"], ["none", "Nothing"]];

const COMMON_PRESETS = [
  ["Escape", "esc"], ["Enter", "enter"], ["Copy", "ctrl+c"], ["Paste", "ctrl+v"], ["Cut", "ctrl+x"],
  ["Undo", "ctrl+z"], ["Redo", WIN ? "ctrl+y" : "ctrl+shift+z"], ["Save", "ctrl+s"], ["Close tab", "ctrl+w"], ["New tab", "ctrl+t"],
  ["Reopen tab", "ctrl+shift+t"], ["Switch window", "alt+tab"]];
const OS_PRESETS = WIN ? [
  ["Task view", "super+tab"], ["Lock screen", "super+l"], ["Screenshot", "super+shift+s"], ["Show desktop", "super+d"],
  ["Desktop ←", "ctrl+super+left"], ["Desktop →", "ctrl+super+right"], ["File Explorer", "super+e"]]
: [["Activities overview", "super"], ["Lock screen", "super+l"], ["Screenshot", "print"],
  ["Workspace ←", "super+pageup"], ["Workspace →", "super+pagedown"]];
const MEDIA_PRESETS = [["Volume +", "volumeup"], ["Volume −", "volumedown"], ["Mute", "mute"], ...(WIN ? [] : [["Mic mute", "micmute"]]),
  ["Play/Pause", "playpause"], ["Next track", "nextsong"], ["Prev track", "previoussong"]];
const VOICE_PRESETS = WIN ? [] : [["Voice start", "super+alt+r"], ["Voice stop & type", "super+alt+d"], ["Voice cancel", "super+alt+x"]];
const KEY_PRESETS = [...COMMON_PRESETS, ...OS_PRESETS, ...MEDIA_PRESETS, ...VOICE_PRESETS];
const CMD_PRESETS = WIN ? [
  ["Calculator", { command: "start calc", release_command: "" }],
  ["Notepad", { command: "start notepad", release_command: "" }],
  ["Lock (rundll32)", { command: "rundll32.exe user32.dll,LockWorkStation", release_command: "" }],
] : [
  ["🎙 Voice: start", { command: "voxtype record start", release_command: "" }],
  ["🎙 Voice: stop & type", { command: "voxtype record stop", release_command: "" }],
  ["🎙 Voice: toggle recording", { command: "voxtype record toggle", release_command: "" }],
  ["🎙 Voice: push-to-talk (hold)", { command: "voxtype record start", release_command: "voxtype record stop" }],
  ["🎙 Voice: cancel", { command: "voxtype record cancel", release_command: "" }],
  ["🎙 Voice panel", { command: "$HOME/.local/bin/voxpanel", release_command: "" }],
  ["Lock session", { command: "loginctl lock-session", release_command: "" }],
  ["Notification test", { command: "notify-send Keybar 'Hello from your Keydial'", release_command: "" }],
];

function select(id) {
  selected = id;
  renderDevice();
  renderEditor();
}

function renderEditor() {
  const ed = $("#editor");
  const b = byId(selected);
  $("#monitor").hidden = !!b;
  ed.hidden = !b;
  if (!b) return;
  const a = b.action;
  const change = (fn, rerender = false) => {
    const before = JSON.stringify(b.action);
    fn();
    if (JSON.stringify(b.action) !== before && (b.note || b.info)) { b.note = b.info = ""; rerender = true; } // edited: the copy notes no longer apply
    save(); renderDevice(); if (rerender) renderEditor();
  };

  const head = el("div", { class: "ed-head" },
    el("input", { class: "emoji", value: b.icon || "", placeholder: "☆", maxlength: 4, title: "Emoji shown on the key",
      oninput: e => change(() => (b.icon = e.target.value.trim())) }),
    el("input", { class: "name", value: b.label, oninput: e => change(() => (b.label = e.target.value)) }),
    el("button", { class: "btn ghost small", title: "Close", onclick: () => select(null) }, "✕"));

  const trig = RECEIVER
    ? el("div", { class: "note" }, "The Keydial is plugged into another computer. When it controls this one, this key runs the action below.")
    : b.trigger
    ? el("div", { class: "section" },
        el("div", { class: "label" }, "Linked Keydial button"),
        el("div", { class: "row" },
          el("span", { class: "chip", title: b.trigger }, prettyTrigger(b.trigger)),
          el("span", { class: "muted small" }, "(what the button sends on its own)"),
          el("div", { style: "flex:1" }),
          el("button", { class: "btn small", onclick: () => startLearn([b.id]) }, "Re-link"),
          el("button", { class: "btn small ghost", onclick: () => change(() => (b.trigger = ""), true) }, "Unlink")))
    : el("div", { class: "linkbox" },
        el("span", { class: "pulse-dot" }),
        el("div", {},
          el("b", {}, "Press the button on your Keydial"),
          el("div", { class: "muted small" }, `…that should be “${b.label}”. It links automatically. You can set the action below already; it starts working once the button is linked.`)));

  const warn = b.note ? el("div", { class: "warnbox" }, "⚠ ", b.note, " ",
    el("button", { class: "btn small ghost", onclick: () => change(() => (b.note = ""), true) }, "Dismiss"))
    : b.info ? el("div", { class: "warnbox info" }, "ℹ ", b.info, " ",
      el("button", { class: "btn small ghost", onclick: () => change(() => (b.info = ""), true) }, "OK")) : null;
  const tabs = el("div", { class: "tabs" }, TYPES.map(([t, name]) =>
    el("button", { class: a.type === t ? "on" : "", onclick: () => change(() => { b.action = { type: t }; }, true) }, name)));

  const body = el("div", { class: "section" }, actionBody(b, change));

  ed.replaceChildren(...[head, trig, warn].filter(Boolean),
    el("div", { class: "section" }, el("div", { class: "label" }, "When pressed"), tabs), body,
    el("div", { class: "foot" },
      el("button", { class: "btn", onclick: testAction, title: "Runs the action in 0.6 s" }, "▶ Test"),
      el("span", { id: "savedMark", class: "saved" }, "Saved ✓"),
      el("span", { id: "edErr", class: "err" }),
      el("div", { class: "spacer", style: "flex:1" }),
      el("button", { class: "btn ghost", onclick: () => select(null) }, "Done")));
  syncAutoLearn();
}

// While an unlinked slot is open, the next Keydial press links to it.
function syncAutoLearn() {
  const b = byId(selected);
  const want = b && !linked(b) && !learn ? b.id : null;
  if (want === autoLearn) return;
  autoLearn = want;
  api("POST", "/api/learn", { start: !!want }).catch(() => {});
}

function actionBody(b, change) {
  const a = b.action;
  const field = (label, input) => el("div", { class: "field" }, el("label", {}, label, input));
  switch (a.type) {
    case "keys": {
      const set = k => change(() => (b.action.keys = k), true);
      const box = captureBox(a.keys || "", set);
      const inp = el("input", { value: a.keys || "", placeholder: "e.g. ctrl+shift+t   or   ctrl+c, ctrl+v",
        onchange: e => set(e.target.value.trim()), onkeydown: e => { if (e.key === "Enter") e.target.blur(); } });
      const typed = el("details", { class: "typed" }, el("summary", {}, "Type it as text instead"),
        inp, el("div", { class: "note" }, "For sequences, e.g. \u201cctrl+c, ctrl+v\u201d (separate with commas). Saved when you press Enter."));
      return [
        box,
        el("div", { class: "note" }, "Click the box and press the keys — Esc, Enter, F-keys and combos all work. The keys are held as long as the Keydial button is."),
        el("div", { class: "label", style: "margin-top:12px" }, "Or pick one"),
        el("div", { class: "presets" }, KEY_PRESETS.map(([n, k]) =>
          el("button", { class: a.keys === k ? "on" : "", title: n, onclick: () => set(k) },
            el("b", {}, prettyShortcut(k)),
            prettyShortcut(k).toLowerCase() === n.toLowerCase() ? null : el("small", {}, n)))),
        typed,
      ];
    }
    case "app": {
      const list = el("div", { class: "apps" }, "Loading apps…");
      const search = el("input", { placeholder: "Search apps…", oninput: () => fill() });
      const fill = () => {
        if (!apps) return;
        const q = search.value.toLowerCase();
        list.replaceChildren(...apps.filter(x => !q || x.name.toLowerCase().includes(q) || x.id.toLowerCase().includes(q)).slice(0, 200).map(x =>
          el("button", { class: x.id === a.app ? "on" : "", onclick: () => change(() => {
              b.action = { type: "app", app: x.id, app_name: x.name };
              if (!b.label || /^K\d+$|^Dial/.test(b.label)) b.label = x.name;
            }, true) },
            x.icon ? el("img", { src: "/api/icon?name=" + encodeURIComponent(x.icon), alt: "", loading: "lazy", onerror: e => e.target.replaceWith(el("span", { class: "noicon" })) }) : el("span", { class: "noicon" }),
            el("span", {}, x.name))));
      };
      loadApps().then(fill);
      return [a.app ? el("div", { class: "note" }, "Opens ", el("b", {}, a.app_name || a.app), ".") : null, search, list];
    }
    case "command":
      return [
        field("Run on press", el("input", { value: a.command || "", placeholder: "e.g. gnome-terminal", oninput: e => change(() => (b.action.command = e.target.value)) })),
        field("Run on release (optional, e.g. push-to-talk)", el("input", { value: a.release_command || "", oninput: e => change(() => (b.action.release_command = e.target.value)) })),
        el("div", { class: "presets" }, CMD_PRESETS.map(([n, v]) => el("button", { onclick: () => change(() => {
          Object.assign(b.action, v);
          if (!b.label || /^K\d+$/.test(b.label)) b.label = n.replace(/^\S+ /, "");
        }, true) }, n))),
        el("div", { class: "note" }, WIN ? "Runs with cmd /C in your session, without a console window; %USERPROFILE% etc. work." : "Runs with sh -c in your session, so ~ and $HOME work."),
      ];
    case "mouse": {
      const sel = el("select", { onchange: e => change(() => (b.action.mouse = e.target.value), true) },
        Object.entries(MOUSE).map(([k, v]) => el("option", { value: k, selected: (a.mouse || "") === k }, v)));
      if (!a.mouse) { b.action.mouse = "left"; save(); }
      const m = a.mouse || "left";
      const extra = [];
      if (m.startsWith("scroll")) extra.push(field("Steps per press", el("input", { type: "number", min: 1, max: 50, value: a.amount || 1, oninput: e => change(() => (b.action.amount = +e.target.value)) })));
      if (m === "move") extra.push(el("div", { class: "grid2" },
        field("Δx (px)", el("input", { type: "number", value: a.amount || 0, oninput: e => change(() => (b.action.amount = +e.target.value)) })),
        field("Δy (px)", el("input", { type: "number", value: a.dy || 0, oninput: e => change(() => (b.action.dy = +e.target.value)) }))));
      return [sel, ...extra, el("div", { class: "note" }, "Clicks are held while the button is held, so you can drag.")];
    }
    case "text":
      return [el("textarea", { placeholder: "Text to type…", oninput: e => change(() => (b.action.text = e.target.value)) }, a.text || ""),
        el("div", { class: "note" }, "Typed as if on a US keyboard (plain ASCII).")];
    case "layout": {
      if (!a.layout) { b.action.layout = "next"; save(); }
      const opts = [["next", "Next layout"], ["prev", "Previous layout"], ...cfg.layouts.map(l => [l.id, `Switch to “${l.name}”`])];
      return [el("select", { onchange: e => change(() => (b.action.layout = e.target.value), true) },
          opts.map(([v, n]) => el("option", { value: v, selected: (a.layout || "next") === v }, n))),
        el("div", { class: "note" }, "Changes which layout is active on this machine. Give every layout a button that switches on, so you can always get back.")];
    }
    case "machine": {
      if (!a.machine) { b.action.machine = "pick"; save(); }
      const opts = [["pick", "Hold, turn the dial to pick, release to switch (tap = previous)"], ["next", "Next machine"], ["self", "This machine"],
        ...((netState && netState.paired) || []).map(p => [p.id, `Switch to “${p.name}”`])];
      return [el("select", { onchange: e => change(() => (b.action.machine = e.target.value), true) },
          opts.map(([v, n]) => el("option", { value: v, selected: (a.machine || "pick") === v }, n))),
        el("div", { class: "note" }, "Chooses which computer the Keydial controls. This button always works on this machine (where the Keydial is plugged in), whatever layout the other computer uses.")];
    }
    case "passthrough":
      return el("div", { class: "note" }, "The button keeps its factory shortcut" + (b.trigger ? ": " + prettyTrigger(b.trigger) : "") + ".");
    default:
      return el("div", { class: "note" }, "The button is silenced.");
  }
}

async function loadApps() {
  if (!apps) apps = await api("GET", "/api/apps").catch(() => []);
  return apps;
}

async function testAction() {
  const b = byId(selected);
  if (!b) return;
  try { await api("POST", "/api/test", b.action); toast("Running “" + b.label + "”…", 1500); }
  catch (e) { toast(e.message); }
}

// ---------- shortcut recorder (browser keyboard) ----------
const CODE = { Escape: "esc", Enter: "enter", NumpadEnter: "kpenter", Space: "space", Tab: "tab", Backspace: "backspace",
  Delete: "delete", Insert: "insert", Home: "home", End: "end", PageUp: "pageup", PageDown: "pagedown",
  ArrowUp: "up", ArrowDown: "down", ArrowLeft: "left", ArrowRight: "right", Minus: "minus", Equal: "equal",
  BracketLeft: "leftbrace", BracketRight: "rightbrace", Backslash: "backslash", Semicolon: "semicolon", Quote: "apostrophe",
  Comma: "comma", Period: "dot", Slash: "slash", Backquote: "grave", PrintScreen: "sysrq", CapsLock: "capslock",
  ContextMenu: "compose", Pause: "pause", ScrollLock: "scrolllock", NumpadAdd: "kpplus", NumpadSubtract: "kpminus",
  NumpadMultiply: "kpasterisk", NumpadDivide: "kpslash", NumpadDecimal: "kpdot", AudioVolumeUp: "volumeup",
  AudioVolumeDown: "volumedown", AudioVolumeMute: "mute", MediaPlayPause: "playpause", MediaTrackNext: "nextsong",
  MediaTrackPrevious: "previoussong" };
const MODCODES = { ControlLeft: "ctrl", ControlRight: "rctrl", ShiftLeft: "shift", ShiftRight: "rshift", AltLeft: "alt",
  AltRight: "altgr", MetaLeft: "super", MetaRight: "rsuper", OSLeft: "super", OSRight: "rsuper" };
function codeToKey(code) {
  if (/^Key[A-Z]$/.test(code)) return code.slice(3).toLowerCase();
  if (/^Digit\d$/.test(code)) return code.slice(5);
  if (/^F\d+$/.test(code)) return code.toLowerCase();
  if (/^Numpad\d$/.test(code)) return "kp" + code.slice(6);
  return CODE[code] || null;
}
function captureBox(initial, onCombo) {
  const box = el("div", { class: "capture", tabindex: "0", role: "button", title: "Click, then press the keys" });
  let keys = initial;
  const show = rec => {
    box.classList.toggle("recording", rec);
    if (rec) box.replaceChildren(el("span", { class: "pulse-dot" }), "Press the shortcut now…  (click elsewhere to cancel)");
    else if (keys) box.replaceChildren(...prettyShortcut(keys).split(", ").map(c => el("span", { class: "chip big" }, c)),
      el("span", { class: "muted small", style: "margin-left:auto" }, "click to change"));
    else box.replaceChildren(el("span", { class: "muted" }, "Click here, then press the keys"));
  };
  show(false);
  // Modifiers come from each event's own flags, so a release the browser never
  // saw (e.g. GNOME swallowed Ctrl+Alt+Q) can't leave Ctrl/Alt "stuck" here.
  const modsOf = e => [e.ctrlKey && "ctrl", e.shiftKey && "shift", e.altKey && "alt", e.metaKey && "super"].filter(Boolean);
  let gotKey = false, modsOnly = [];
  let abort = null;
  const finish = c => {
    if (abort) { abort.abort(); abort = null; }
    keys = c; box.blur(); onCombo(c);
  };
  box.addEventListener("focus", () => {
    gotKey = false; modsOnly = []; show(true);
    // The daemon also listens to the physical keyboard, so shortcuts GNOME
    // swallows (Ctrl+Alt+Q, Super+L, Print…) still get recorded.
    abort = new AbortController();
    const mine = abort;
    fetch("/api/capture", { method: "POST", headers: { "X-Token": TOKEN }, signal: mine.signal })
      .then(r => r.json()).then(d => { if (abort === mine && d.keys) finish(d.keys); })
      .catch(() => {});
  });
  box.addEventListener("blur", () => { if (abort) { abort.abort(); abort = null; } show(false); });
  box.addEventListener("keydown", e => {
    e.preventDefault(); e.stopPropagation();
    if (MODCODES[e.code]) { const m = modsOf(e); if (m.length > modsOnly.length) modsOnly = m; return; }
    const k = codeToKey(e.code);
    if (!k) return;
    gotKey = true; finish([...modsOf(e), k].join("+"));
  });
  box.addEventListener("keyup", e => {
    e.preventDefault();
    // A modifier pressed and released on its own (e.g. Super) once all are up.
    if (MODCODES[e.code] && !gotKey && modsOnly.length && modsOf(e).length === 0) finish(modsOnly.join("+"));
  });
  return box;
}

// ---------- learning ----------
async function startLearn(queue) {
  learn = { queue: [...queue] };
  autoLearn = null;
  $("#learnDlg").showModal();
  nextLearn();
}
async function nextLearn() {
  const id = learn.queue.shift();
  if (!id) return endLearn();
  learn.id = id;
  const b = byId(id);
  selected = id; renderDevice(); renderEditor();
  $("#learnTitle").textContent = `Press “${b.label}” on your Keydial…`;
  const remaining = learn.queue.length;
  $("#learnSub").textContent = remaining ? `${remaining} more after this one.` : (id.startsWith("dial") ? "Turn or press the dial the matching way." : "It will be assigned to this slot.");
  $("#learnSkip").hidden = !remaining;
  await api("POST", "/api/learn", { start: true }).catch(e => toast(e.message));
}
async function endLearn() {
  learn = null;
  await api("POST", "/api/learn", { start: false }).catch(() => {});
  if ($("#learnDlg").open) $("#learnDlg").close();
  renderDevice(); renderEditor();
}
function onLearned(sig) {
  const id = learn && learn.id ? learn.id : autoLearn;
  if (!id) return;
  const auto = !learn;
  autoLearn = null;
  const b = byId(id);
  for (const o of bindings()) if (o.id !== b.id && o.trigger === sig) {
    o.trigger = ""; toast(`Moved from “${o.label}” to “${b.label}”`);
  }
  b.trigger = sig;
  save(true);
  renderDevice();
  flash(b.id);
  if (auto) { toast(`Linked ${prettyTrigger(sig)} → “${b.label}”`); renderEditor(); }
  else setTimeout(nextLearn, 250);
}
// The daemon forgets learn mode after a minute; keep it alive while waiting, and drop it when the window closes.
setInterval(() => { if (learn || autoLearn) api("POST", "/api/learn", { start: true }).catch(() => {}); }, 30000);
addEventListener("pagehide", () => {
  if (learn || autoLearn) navigator.sendBeacon("/api/learn?token=" + TOKEN, new Blob([JSON.stringify({ start: false })], { type: "application/json" }));
});
$("#learnCancel").onclick = endLearn;
$("#learnSkip").onclick = () => nextLearn();
$("#learnDlg").addEventListener("cancel", e => { e.preventDefault(); }); // Esc may be a device key
$("#learnAll").onclick = () => startLearn(cfg.slots.map(s => s.id));
if (RECEIVER) $("#learnAll").hidden = true;

// ---------- live events ----------
function addLog(ev) {
  // Describe the key in the layout that actually ran it, not the one being edited.
  const slot = ev.binding && cfg.slots.find(s => s.id === ev.binding);
  const L = cfg.layouts.find(l => l.id === ev.layout) || curLayout();
  const k = slot && L.keys[slot.id];
  const b = k && { label: k.label, action: k.action, trigger: slot.trigger };
  const where = L.id !== curLayout().id || cfg.layouts.length > 1 ? " · layout “" + L.name + "”" : "";
  const li = el("li", {},
    el("time", {}, new Date().toLocaleTimeString()),
    el("span", { class: "chip" }, ev.trigger ? prettyTrigger(ev.trigger) : "🖧"),
    ev.to ? el("span", {}, "→ sent to ", el("b", {}, ev.to), el("span", { class: "muted" }, " (runs with its layout)"))
    : b ? el("span", {}, "→ ", el("b", {}, b.label), el("span", { class: "muted" }, " · " + summary(b) + where + (ev.from ? " · from " + ev.from : ""))) : el("span", { class: "muted" }, "not assigned"));
  const log = $("#log");
  log.prepend(li);
  while (log.children.length > 60) log.lastChild.remove();
}
function connectEvents() {
  const es = new EventSource("/api/events?token=" + TOKEN);
  es.onmessage = m => {
    const ev = JSON.parse(m.data);
    if (ev.type === "learned") onLearned(ev.trigger);
    else if (ev.type === "layout") {
      const was = cfg.active_layout;
      cfg.active_layout = ev.layout;
      if (!editLayout || editLayout === was) editLayout = ev.layout; // follow the switch unless editing another one
      renderAll();
      toast("Layout: " + layoutName(ev.layout), 1500);
    }
    else if (ev.type === "trigger") {
      addLog(ev);
      if (ev.binding && !ev.to) flash(ev.binding);
      // The daemon wasn't in learn mode (e.g. it just restarted), but the panel
      // is waiting for a button: link this one anyway.
      else if (learn && learn.id || autoLearn) onLearned(ev.trigger);
      else if (!learn && !autoLearn) toast(`The “${prettyTrigger(ev.trigger)}” button isn't linked to a slot yet — click an empty slot on the left, then press it again`, 4000);
    } else if (ev.type === "net") refreshNet();
    else if (ev.type === "status" || ev.type === "hello") {
      if (ev.type === "hello") refreshNet();
      refreshStatus();
      // A reconnect means the daemon may have restarted and forgotten learn mode.
      if (ev.type === "hello" && (learn || autoLearn)) api("POST", "/api/learn", { start: true }).catch(() => {});
    }
  };
  es.onerror = () => { $("#status").textContent = "Keybar not running"; $("#status").className = "pill bad"; };
}

// ---------- machines ----------
const OSNAME = { linux: "Linux", windows: "Windows", darwin: "macOS" };
function machineName(id) {
  if (!id) return netState ? netState.me.name : "this machine";
  const p = netState && netState.paired.find(p => p.id === id);
  return p ? p.name : "unknown machine";
}
async function refreshNet() {
  try { netState = await api("GET", "/api/net"); } catch { return; }
  const t = netState.target, rm = netState.remote;
  const pill = $("#targetPill");
  if (RECEIVER) { // the Keydial is on another machine: show where it points
    pill.textContent = !rm ? "Keydial: not connected" : rm.here ? `Keydial (${rm.host}) → this machine` : `Keydial (${rm.host}) → ${rm.target}`;
    pill.classList.toggle("remote", !!rm && !rm.here);
  } else {
    pill.textContent = "Keydial → " + (t ? machineName(t) : "this machine");
    pill.classList.toggle("remote", !!t);
  }
  if ($("#machinesDlg").open) renderMachines();
  renderPairing();
  if ($("#copyDlg").open && !copyList) loadCopyList(); // e.g. the session came up after pairing
}
function mrow(m, ...buttons) {
  return el("div", { class: "mrow" }, el("span", { class: "dot" + (m.online ? " on" : ""), title: m.online ? "Connected" : "Offline" }),
    el("div", { style: "flex:1;min-width:0" }, el("div", { class: "nm" }, m.name),
      el("div", { class: "sub" }, [OSNAME[m.os] || m.os, m.addr].filter(Boolean).join(" · "))), ...buttons);
}
function renderMachines() {
  const st = netState;
  if (document.activeElement !== $("#myName")) $("#myName").value = st.me.name;
  const netCall = (path, body) => api("POST", path, body).then(s => { netState = s; refreshNet(); }).catch(e => toast(e.message));
  const rm = st.remote;
  // A receiver can't choose the target (the Keydial isn't here); it just shows it.
  const meTag = RECEIVER ? (rm && rm.here ? el("span", { class: "tag" }, "Controlled now") : null)
    : st.target === "" ? el("span", { class: "tag" }, "Keydial here")
    : el("button", { class: "btn small", onclick: () => netCall("/api/net/target", { id: "" }) }, "Control this");
  const me = mrow({ ...st.me, addr: "" }, ...[meTag].filter(Boolean));
  $("#pairedList").replaceChildren(me, ...st.paired.map(p => mrow(p,
    RECEIVER ? (rm && rm.host_id === p.id ? el("span", { class: "tag" }, rm.here ? "Keydial → here" : "Keydial → " + rm.target) : null)
    : st.target === p.id ? el("span", { class: "tag" }, "Keydial here")
      : p.online ? el("button", { class: "btn small", onclick: () => netCall("/api/net/target", { id: p.id }) }, "Control") : null,
    el("button", { class: "btn small ghost", onclick: () => { if (confirm(`Unpair “${p.name}”? You'll need to pair again to use it.`)) netCall("/api/net/unpair", { id: p.id }); } }, "Unpair"))));
  $("#nearbyList").replaceChildren(...(st.nearby.length ? st.nearby.map(m => mrow(m,
    el("button", { class: "btn small primary", onclick: () => netCall("/api/net/pair", { id: m.id }) }, "Pair…")))
    : [el("div", { class: "empty" }, "Looking for other computers running Keybar…")]));
}
let pairShown = null;
function renderPairing() {
  const p = netState && netState.pairing, dlg = $("#pairDlg");
  if (!p) {
    if (dlg.open) dlg.close();
    if (pairShown) {
      const p = netState.paired.find(x => x.id === pairShown);
      if (p) openCopy(p.id, `Paired with ${p.name} ✓. Do you want to copy its layouts to this machine? Shortcuts are converted for this system where needed; anything that can't be converted is marked ⚠. You can also do this later with “Copy from…”.`);
      else toast("Pairing cancelled");
      pairShown = null;
    }
    return;
  }
  pairShown = p.peer;
  $("#pairTitle").textContent = (p.outgoing ? "Pairing with " : "Pairing request from ") + p.name;
  $("#pairCode").textContent = p.code;
  const them = p.theirs === "yes" ? `${p.name} confirmed.` : p.theirs === "no" ? `${p.name} cancelled.` : `Waiting for ${p.name}…`;
  $("#pairState").textContent = (p.mine === "yes" ? "You confirmed. " : "") + them;
  $("#pairYes").disabled = !!p.mine;
  if (!dlg.open) dlg.showModal();
}
const answer = ok => api("POST", "/api/net/answer", { id: netState.pairing.peer, ok }).catch(e => toast(e.message));
$("#pairYes").onclick = () => answer(true);
$("#pairNo").onclick = () => answer(false);
$("#pairDlg").addEventListener("cancel", e => { e.preventDefault(); answer(false); });
$("#machinesBtn").onclick = $("#targetPill").onclick = () => { renderMachines(); $("#machinesDlg").showModal(); };
$("#mClose").onclick = () => $("#machinesDlg").close();
$("#myNameSave").onclick = () => api("POST", "/api/net/name", { name: $("#myName").value }).then(() => { toast("Renamed"); refreshNet(); }).catch(e => toast(e.message));

// ---------- copying layouts from another machine ----------
let copyFrom = null, copyList = null;
function openCopy(peerId, intro) {
  const online = ((netState && netState.paired) || []).filter(p => p.online);
  copyFrom = peerId || copyFrom || (online[0] && online[0].id) || null;
  copyList = null;
  $("#copyIntro").textContent = intro || "Copied layouts are added next to yours; nothing you have is changed. Shortcuts are converted for this system where needed, and anything that can't be converted is marked ⚠.";
  if (!$("#copyDlg").open) $("#copyDlg").showModal();
  loadCopyList();
}
async function loadCopyList() {
  const paired = (netState && netState.paired) || [];
  $("#copyMachine").replaceChildren(...(paired.length ? paired.map(p => el("option", { value: p.id, selected: p.id === copyFrom }, p.name + (p.online ? "" : " (offline)")))
    : [el("option", { value: "" }, "No paired machines")]));
  const list = $("#copyLayouts");
  $("#copyGo").disabled = true;
  const p = paired.find(p => p.id === copyFrom);
  if (!p) { list.replaceChildren(el("div", { class: "empty" }, "Pair a machine first (🖧 Machines).")); return; }
  if (!p.online) { list.replaceChildren(el("div", { class: "empty" }, `Waiting for ${p.name} to connect…`)); return; }
  list.replaceChildren(el("div", { class: "empty" }, "Loading…"));
  try {
    const r = await api("GET", "/api/net/layouts?id=" + encodeURIComponent(p.id));
    copyList = r;
    list.replaceChildren(...r.layouts.map(l => el("label", { class: "check" },
      el("input", { type: "checkbox", value: l.id, checked: true, onchange: syncCopyBtn }),
      el("span", {}, el("b", {}, l.name), el("span", { class: "muted small" }, ` · ${l.assigned} key${l.assigned === 1 ? "" : "s"} assigned · ${OSNAME[r.os] || r.os}`)))));
    syncCopyBtn();
  } catch (e) { list.replaceChildren(el("div", { class: "empty" }, e.message)); }
}
function syncCopyBtn() { $("#copyGo").disabled = !document.querySelector("#copyLayouts input:checked"); }
$("#copyMachine").onchange = e => { copyFrom = e.target.value; loadCopyList(); };
$("#copyClose").onclick = () => $("#copyDlg").close();
$("#copyGo").onclick = async () => {
  const ids = [...document.querySelectorAll("#copyLayouts input:checked")].map(i => i.value);
  $("#copyGo").disabled = true;
  try {
    await save(true);
    const r = await api("POST", "/api/net/import", { id: copyFrom, layouts: ids });
    cfg = r.config; editLayout = r.first; selected = null;
    if (r.activated) cfg.active_layout = r.first;
    renderAll();
    $("#copyDlg").close();
    const rp = r.report;
    const n = (x, w) => `${x} ${w}${x === 1 ? "" : "s"}`;
    toast(`Copied ${n(rp.layouts, "layout")}: ${n(rp.copied, "key")} as-is, ${rp.converted} converted` +
      (rp.attention ? `, ${rp.attention} need attention (⚠)` : "") +
      (r.activated ? ". It's now the active layout." : ". It isn't active yet: click “Make active” to use it."), 8000);
  } catch (e) { toast(e.message); syncCopyBtn(); }
};

// ---------- status & settings ----------
async function refreshStatus() {
  try { status = await api("GET", "/api/status"); } catch { return; }
  const p = $("#status");
  if (RECEIVER) { p.textContent = "Receives from paired machines"; p.className = "pill ok"; }
  else if (status.paused) { p.textContent = "Paused"; p.className = "pill"; }
  else if (status.devices.length) {
    p.textContent = "Keydial connected" + (status.battery >= 0 ? ` · 🔋 ${status.battery}%` : "");
    p.className = "pill ok";
  } else { p.textContent = "Keydial not connected"; p.className = "pill bad"; }
  const ban = $("#banner");
  if (!status.uinput) {
    ban.hidden = false;
    ban.replaceChildren(el("b", {}, "One-time setup needed. "),
      "Keybar needs access to /dev/uinput to send keys and mouse events. Run this in a terminal once: ",
      el("code", {}, "sudo ~/keybar/install-udev.sh"),
      ". Apps and commands already work. Keybar picks up the change on its own within a few seconds.");
  } else ban.hidden = true;
}

function openSettings() {
  $("#setGrab").checked = cfg.grab;
  $("#setPass").checked = cfg.unmapped_passthrough;
  $("#setVendor").value = cfg.device.vendor || "";
  $("#setProduct").value = cfg.device.product || "";
  $("#setName").value = cfg.device.name_contains || "";
  const devs = (status && status.devices || []).map(d => `${d.path} — ${d.name}${d.grabbed ? " (taken over)" : ""}`).join("\n");
  $("#setInfo").innerText = `Config: ${status ? status.config_path : ""}\nVersion ${status ? status.version : ""}\n${devs || "No matching device open."}`;
  $("#settingsDlg").showModal();
}
$("#settingsBtn").onclick = openSettings;
$("#setClose").onclick = () => $("#settingsDlg").close();
$("#setSave").onclick = async () => {
  cfg.grab = $("#setGrab").checked;
  cfg.unmapped_passthrough = $("#setPass").checked;
  cfg.device = { vendor: $("#setVendor").value.trim(), product: $("#setProduct").value.trim(), name_contains: $("#setName").value.trim() };
  await save(true);
  $("#settingsDlg").close();
  setTimeout(refreshStatus, 800);
};

// ---------- one panel at a time ----------
// A newly opened panel tells older ones (same browser) to close; if the browser
// won't let a window close itself, it shows a "open elsewhere" screen instead.
const panelChannel = new BroadcastChannel("keybar-panel");
function takeOver() { $("#elsewhere").hidden = true; panelChannel.postMessage("opened"); }
panelChannel.onmessage = e => {
  if (e.data !== "opened") return;
  if (learn || autoLearn) { learn = null; autoLearn = null; api("POST", "/api/learn", { start: false }).catch(() => {}); }
  if ($("#learnDlg").open) $("#learnDlg").close();
  window.close();
  setTimeout(() => { $("#elsewhere").hidden = false; }, 250);
};
$("#useHere").onclick = () => { takeOver(); refreshStatus(); };
takeOver();

// ---------- boot ----------
(async () => {
  cfg = await api("GET", "/api/config");
  editLayout = cfg.active_layout;
  renderLayouts();
  renderDevice();
  if (location.hash && byId(location.hash.slice(1))) select(location.hash.slice(1));
  addEventListener("hashchange", () => { const id = location.hash.slice(1); if (byId(id)) select(id); });
  connectEvents();
  refreshStatus();
  await refreshNet();
  setInterval(refreshStatus, 30000);
  loadApps().then(renderDevice);
})();
