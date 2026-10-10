// Sessions ouvertes en onglets : terminaux SSH (xterm.js) et bureaux VNC/RDP (noVNC).
//
// Chaque session garde sa propre connexion et son propre affichage. Les onglets
// inactifs restent connectés : ils sont seulement masqués (visibility), sans changer
// de taille, pour ne pas provoquer de redimensionnement du bureau distant.
import RFB from "/vendor/novnc/core/rfb.js";

const $ = (s) => document.querySelector(s);
const sessions = [];
let active = null;
// Affichage : "tabs" (une session à la fois) ou "grid" (mosaïque, toutes visibles).
let layout = "tabs";
try { if (localStorage.getItem("gna.layout") === "grid") layout = "grid"; } catch { /* stockage indisponible */ }
// Saisie synchronisée : la frappe dans un terminal SSH part dans tous les terminaux inclus.
let syncOn = false;
// En mosaïque, pas de transfert de fichiers : on ne saurait pas avec certitude vers
// quelle machine il partirait.
const gridActive = () => layout === "grid" && sessions.length > 1;
let seq = 0;
// Liens avec app.js : lus au moment de l'usage (window.GnaHooks est défini par app.js
// dès son chargement), donc indépendants de l'ordre de chargement des scripts.
const ctx = {
  get tools() { return (window.GnaHooks && window.GnaHooks.tools()) || {}; },
  get recording() { return (window.GnaHooks && window.GnaHooks.recording()) || {}; },
  onHome() { if (window.GnaHooks) window.GnaHooks.onHome(); },
  onChange(n) { if (window.GnaHooks) window.GnaHooks.onChange(n); },
};

const randomId = () => {
  const b = new Uint8Array(16);
  crypto.getRandomValues(b);
  return Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");
};
const el = (tag, cls, text) => {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text != null) e.textContent = text;
  return e;
};
const fmtSize = (n) => n < 1024 ? `${n} o` : n < 1048576 ? `${(n / 1024).toFixed(1)} Ko` : n < 1073741824 ? `${(n / 1048576).toFixed(1)} Mo` : `${(n / 1073741824).toFixed(2)} Go`;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const PROTO = { ssh: "SSH", vnc: "VNC", rdp: "RDP" };

// Couleurs du terminal et du fond des bureaux : celles du thème choisi (style.css).
function cssVar(name, fallback) {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim() || fallback;
}
// Palettes ANSI (couleurs des commandes : ls, invite…) propres à certains thèmes.
const ANSI = {
  nord: ["#3b4252", "#bf616a", "#a3be8c", "#ebcb8b", "#81a1c1", "#b48ead", "#88c0d0", "#e5e9f0",
    "#4c566a", "#bf616a", "#a3be8c", "#ebcb8b", "#81a1c1", "#b48ead", "#8fbcbb", "#eceff4"],
  dracula: ["#21222c", "#ff5555", "#50fa7b", "#f1fa8c", "#bd93f9", "#ff79c6", "#8be9fd", "#f8f8f2",
    "#6272a4", "#ff6e6e", "#69ff94", "#ffffa5", "#d6acff", "#ff92df", "#a4ffff", "#ffffff"],
  gruvbox: ["#282828", "#cc241d", "#98971a", "#d79921", "#458588", "#b16286", "#689d6a", "#a89984",
    "#928374", "#fb4934", "#b8bb26", "#fabd2f", "#83a598", "#d3869b", "#8ec07c", "#ebdbb2"],
  solarized: ["#073642", "#dc322f", "#859900", "#b58900", "#268bd2", "#d33682", "#2aa198", "#eee8d5",
    "#002b36", "#cb4b16", "#586e75", "#657b83", "#839496", "#6c71c4", "#93a1a1", "#fdf6e3"],
};
const ANSI_NAMES = ["black", "red", "green", "yellow", "blue", "magenta", "cyan", "white",
  "brightBlack", "brightRed", "brightGreen", "brightYellow", "brightBlue", "brightMagenta", "brightCyan", "brightWhite"];

function termTheme() {
  const theme = {
    background: cssVar("--term-bg", "#0b0e13"),
    foreground: cssVar("--term-fg", "#e6e9ef"),
    cursor: cssVar("--term-cursor", "#5fb3a1"),
    selectionBackground: cssVar("--term-sel", "#2a3a37"),
  };
  const palette = ANSI[document.documentElement.dataset.theme];
  if (palette) palette.forEach((c, i) => { theme[ANSI_NAMES[i]] = c; });
  return theme;
}
window.addEventListener("gna-theme", () => {
  const theme = termTheme();
  for (const s of sessions) {
    if (s.kind === "ssh") s.xterm.options.theme = theme;
    if (s.kind === "desk" && s.rfb) s.rfb.background = theme.background;
  }
});

let toastTimer = null;
function toast(text) {
  const t = $("#desk-toast");
  t.textContent = text;
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.hidden = true; }, 4000);
}

async function api(url, opts) {
  const res = await fetch(url, { credentials: "same-origin", ...opts });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    const e = new Error(data.error || `Erreur ${res.status}`);
    e.status = res.status;
    throw e;
  }
  return data;
}

// Envoi d'un fichier avec barre de progression (corps brut).
function uploadXHR(url, file, bar) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open("POST", url);
    xhr.setRequestHeader("Content-Type", "application/octet-stream");
    xhr.setRequestHeader("X-Gna-Upload", "1");
    xhr.upload.onprogress = (e) => {
      if (!e.lengthComputable) return;
      const pct = Math.round((e.loaded / e.total) * 100);
      bar.firstElementChild.style.width = pct + "%";
      bar.lastElementChild.textContent = `${file.name} — ${pct} %`;
    };
    xhr.onload = () => {
      let data = {};
      try { data = JSON.parse(xhr.responseText); } catch { /* réponse non JSON */ }
      if (xhr.status === 200) return resolve(data);
      const e = new Error(data.error || `Erreur ${xhr.status}`);
      e.status = xhr.status;
      reject(e);
    };
    xhr.onerror = () => reject(new Error("envoi interrompu"));
    xhr.send(file);
  });
}

// ======================================================================
// Session (commun)
// ======================================================================

class Session {
  constructor(target) {
    this.id = ++seq;
    this.target = target;
    this.mode = "";
    this.statusText = "connexion…";
    this.recorded = false;
    this.panel = null; // panneau latéral ouvert pour cette session
    this.pane = el("div", "pane");
    this.pane.dataset.session = String(this.id);
    $("#panes").append(this.pane);
    this.buildTab();
    // En-tête du volet, visible en mosaïque.
    this.head = el("div", "pane-head");
    this.headDot = el("span", "dot");
    this.head.append(this.headDot, el("span", "stab-proto", PROTO[this.target.protocol] || ""), el("span", "pane-name", this.label));
    this.pane.append(this.head);
    // En mosaïque, un clic sur un volet en fait la session active.
    this.pane.addEventListener("pointerdown", () => { if (active !== this) activate(this); }, true);
  }

  buildTab() {
    const tab = el("div", "stab");
    tab.setAttribute("role", "presentation");
    const main = el("button", "stab-main");
    main.type = "button";
    main.setAttribute("role", "tab");
    main.dataset.action = "ses-switch";
    main.dataset.session = String(this.id);
    this.dot = el("span", "dot");
    // Même machine ouverte plusieurs fois : « nom 2 », « nom 3 »…
    const same = sessions.filter((s) => s.target.id === this.target.id).length;
    this.label = same ? `${this.target.name} ${same + 1}` : this.target.name;
    main.append(this.dot, el("span", "stab-proto", PROTO[this.target.protocol] || ""), el("span", "stab-name", this.label));
    this.recBadge = el("span", "stab-rec", "●");
    this.recBadge.title = "Session enregistrée";
    this.recBadge.hidden = true;
    main.append(this.recBadge);
    const close = el("button", "stab-close", "✕");
    close.type = "button";
    close.dataset.action = "ses-tab-close";
    close.dataset.session = String(this.id);
    close.setAttribute("aria-label", `Fermer ${this.label}`);
    close.title = "Fermer cette session";
    tab.append(main, close);
    this.tab = tab;
    this.tabMain = main;
    $("#stabs").append(tab);
  }

  setStatus(text, mode) {
    this.statusText = text;
    this.mode = mode;
    this.dot.className = "dot" + (mode ? " " + mode : "");
    if (this.headDot) this.headDot.className = this.dot.className;
    this.tabMain.title = `${this.label} — ${text}`;
    this.recBadge.hidden = !this.recorded;
    if (this === active) renderToolbar();
  }

  show(on) {
    this.pane.classList.toggle("active", on);
    this.tabMain.setAttribute("aria-selected", String(on));
    this.tab.classList.toggle("active", on);
  }

  destroy() {
    this.pane.remove();
    this.tab.remove();
  }
}

// ======================================================================
// Terminal SSH
// ======================================================================

class SshSession extends Session {
  constructor(target) {
    super(target);
    this.kind = "ssh";
    this.sync = true; // inclus dans la saisie synchronisée
    this.sftp = { tid: null, cwd: "" };
    const lab = el("label", "pane-sync");
    this.syncBox = el("input");
    this.syncBox.type = "checkbox";
    this.syncBox.checked = true;
    this.syncBox.addEventListener("change", () => { this.sync = this.syncBox.checked; renderSync(); });
    lab.append(this.syncBox, document.createTextNode(" synchro"));
    lab.title = "Inclure ce terminal dans la saisie synchronisée";
    this.head.append(lab);
    this.box = el("div", "terminal");
    this.pane.append(this.box);
    this.xterm = new Terminal({
      cursorBlink: true,
      fontFamily: getComputedStyle(document.documentElement).getPropertyValue("--mono"),
      fontSize: 14,
      scrollback: 5000,
      theme: termTheme(),
    });
    this.fit = new FitAddon.FitAddon();
    this.xterm.loadAddon(this.fit);
    this.xterm.open(this.box);
    this.resize();
    this.connect();
  }

  get sftpEnabled() { return Boolean(ctx.tools.sftp); }

  connect() {
    const x = this.xterm;
    this.setStatus("connexion…", "");
    const proto = location.protocol === "https:" ? "wss" : "ws";
    this.sftp = { tid: this.sftpEnabled ? randomId() : null, cwd: "" };
    const params = new URLSearchParams({ cols: x.cols, rows: x.rows });
    if (this.sftp.tid) params.set("t", this.sftp.tid);
    const ws = new WebSocket(`${proto}://${location.host}/api/targets/${this.target.id}/ssh?${params}`);
    ws.binaryType = "arraybuffer";
    this.ws = ws;
    const enc = new TextEncoder();

    ws.onmessage = (ev) => {
      if (typeof ev.data !== "string") return x.write(new Uint8Array(ev.data));
      const m = JSON.parse(ev.data);
      if (m.type === "connected") {
        this.recorded = Boolean(m.recording);
        this.setStatus(`${this.target.username}@${this.target.host}`, "on");
        if (this === active && this.panel === "sftp") sftpList(this, this.sftp.cwd);
      } else if (m.type === "error") {
        x.write(`\r\n\x1b[31m[Gná] ${m.message}\x1b[0m\r\n`);
        this.setStatus(m.message, "off");
      }
    };
    ws.onclose = () => {
      if (this.ws !== ws) return;
      if (this.mode === "on") {
        x.write("\r\n\x1b[90m[Gná] Session terminée.\x1b[0m\r\n");
        this.setStatus("déconnecté", "off");
      } else if (this.mode !== "off") {
        this.setStatus("connexion impossible", "off");
      }
    };
    const disposers = [
      x.onData((d) => this.input(enc.encode(d))),
      x.onBinary((d) => this.input(Uint8Array.from(d, (c) => c.charCodeAt(0)))),
      x.onResize(({ cols, rows }) => ws.readyState === 1 && ws.send(JSON.stringify({ type: "resize", cols, rows }))),
    ];
    ws.addEventListener("close", () => disposers.forEach((d) => d.dispose()));
  }

  // Frappe dans ce terminal : envoyée ici, et aux autres terminaux synchronisés.
  input(bytes) {
    const targets = syncOn && this.sync ? sessions.filter((t) => t.kind === "ssh" && t.sync) : [this];
    for (const t of targets) if (t.ws && t.ws.readyState === 1) t.ws.send(bytes);
  }

  reconnect() {
    this.disconnect();
    this.xterm.reset();
    this.connect();
  }

  disconnect() {
    if (this.ws) {
      const ws = this.ws;
      this.ws = null;
      ws.close();
    }
  }

  focus() { this.xterm.focus(); }

  resize() {
    try { this.fit.fit(); } catch { /* terminal pas encore affiché */ }
  }

  destroy() {
    this.disconnect();
    this.xterm.dispose();
    super.destroy();
  }
}

// ======================================================================
// Bureau à distance (VNC / RDP)
// ======================================================================

const K = { ctrl: [0xffe3, "ControlLeft"], alt: [0xffe9, "AltLeft"], win: [0xffeb, "MetaLeft"], del: [0xffff, "Delete"],
  tab: [0xff09, "Tab"], esc: [0xff1b, "Escape"], print: [0xff61, "PrintScreen"], l: [0x6c, "KeyL"] };
const COMBOS = {
  cad: ["ctrl", "alt", "del"], win: ["win"], alttab: ["alt", "tab"], ctrlesc: ["ctrl", "esc"], winl: ["win", "l"], print: ["print"],
};

const isPasteKey = (ev) =>
  ((ev.ctrlKey || ev.metaKey) && !ev.altKey && (ev.code === "KeyV" || ev.key === "v" || ev.key === "V")) ||
  (ev.shiftKey && ev.code === "Insert");

class DeskSession extends Session {
  constructor(target) {
    super(target);
    this.kind = "desk";
    this.screen = el("div", "screen");
    this.msg = el("div", "pane-msg");
    this.msg.hidden = true;
    this.msgText = el("p");
    const again = el("button", "", "Reconnecter");
    again.type = "button";
    again.dataset.action = "ses-reconnect";
    this.msg.append(this.msgText, again);
    this.pane.append(this.screen, this.msg);
    // Copier-coller direct (voir onPasteKey).
    this.lastRemote = "";
    this.lastSent = "";
    this.pendingPaste = null;
    this.swallowKeyUp = false;
    this.screen.addEventListener("keydown", (ev) => this.onPasteKey(ev), true);
    this.screen.addEventListener("keyup", (ev) => this.onPasteKeyUp(ev), true);
    this.connect();
  }

  get filesEnabled() { return Boolean(ctx.tools.file_transfer) && this.target.protocol === "rdp"; }
  get clipboard() { return Boolean(ctx.tools.clipboard); }

  setStatus(text, mode) {
    super.setStatus(text, mode);
    this.msgText.textContent = text;
    this.msg.hidden = mode !== "off";
  }

  connect() {
    this.screen.replaceChildren();
    this.recorded = Boolean(ctx.recording.vnc);
    this.setStatus("connexion…", "");
    const proto = location.protocol === "https:" ? "wss" : "ws";
    // Taille initiale = zone d'affichage ; elle suit ensuite la fenêtre.
    const params = new URLSearchParams({ w: Math.round(this.screen.clientWidth), h: Math.round(this.screen.clientHeight) });
    this.transferId = this.filesEnabled ? randomId() : null;
    if (this.transferId) params.set("t", this.transferId);
    const r = new RFB(this.screen, `${proto}://${location.host}/api/targets/${this.target.id}/vnc?${params}`, { wsProtocols: ["binary"] });
    r.scaleViewport = true;  // si la cible ne sait pas changer de taille, l'image est mise à l'échelle
    r.resizeSession = layout !== "grid"; // sinon, le bureau distant prend la taille de la zone (pas en mosaïque)
    r.background = cssVar("--term-bg", "#0b0e13");
    r.focusOnClick = true;
    let failed = false;
    this.connected = true;
    r.addEventListener("connect", () => {
      this.setStatus(`${this.target.host}:${this.target.port}`, "on");
      if (this === active) {
        r.focus();
        if (this.panel === "files") filesRefresh();
      }
    });
    r.addEventListener("securityfailure", (e) => {
      failed = true;
      this.setStatus(e.detail.reason || "connexion refusée", "off");
    });
    r.addEventListener("disconnect", () => {
      if (this.rfb !== r) return;
      this.connected = false;
      if (!failed) this.setStatus(this.mode === "on" ? "déconnecté" : "connexion impossible", "off");
    });
    r.addEventListener("clipboard", (e) => this.clipReceived(e.detail.text));
    this.rfb = r;
  }

  disconnect() {
    if (this.rfb) {
      const r = this.rfb;
      this.rfb = null;
      if (this.connected) r.disconnect();
    }
    this.connected = false;
    this.transferId = null;
    this.pendingPaste = null;
  }

  reconnect() {
    this.disconnect();
    this.connect();
  }

  focus() { if (this.rfb) this.rfb.focus(); }
  resize() { /* noVNC suit lui-même la taille de la zone */ }

  destroy() {
    this.disconnect();
    super.destroy();
  }

  sendCombo(name) {
    if (!this.rfb || !this.connected) return;
    if (name === "cad") return this.rfb.sendCtrlAltDel();
    const keys = COMBOS[name].map((k) => K[k]);
    for (const [sym, code] of keys) this.rfb.sendKey(sym, code, true);
    for (const [sym, code] of keys.slice().reverse()) this.rfb.sendKey(sym, code, false);
    this.rfb.focus();
  }

  // ----- presse-papiers direct -----
  // Copier sur la machine distante remplit le presse-papiers du poste ; Ctrl+V (ou
  // Maj+Inser) sur le bureau distant y colle le presse-papiers du poste, récupéré par
  // l'événement « paste » natif du navigateur (aucune autorisation demandée).

  clipReceived(text) {
    if (!this.clipboard || text === this.lastRemote) return;
    this.lastRemote = text;
    const fallback = () => {
      const onCopy = (ev) => { ev.clipboardData.setData("text/plain", text); ev.preventDefault(); };
      document.addEventListener("copy", onCopy, { once: true });
      let ok = false;
      try { ok = document.execCommand("copy"); } catch { ok = false; }
      document.removeEventListener("copy", onCopy);
      if (ok) this.lastSent = text;
      else if (this === active) toast("Le navigateur a bloqué la copie : recopiez sur le bureau distant (Ctrl+C).");
    };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(() => { this.lastSent = text; }, fallback);
    } else {
      fallback();
    }
  }

  onPasteKey(ev) {
    if (!this.clipboard || !this.rfb || !this.connected || !isPasteKey(ev)) return;
    ev.stopImmediatePropagation(); // pas de preventDefault : le navigateur doit coller
    this.swallowKeyUp = true;
    if (this.pendingPaste) return; // répétition automatique de la touche
    const key = ev.code === "Insert" ? [0xff63, "Insert"] : [0x76, "KeyV"];
    pasteTarget = this;
    // Sans événement « paste » (presse-papiers vide, image…), la touche part quand même.
    this.pendingPaste = { key, timer: setTimeout(() => this.finishPaste(null), 400) };
  }

  async finishPaste(text) {
    if (!this.pendingPaste) return;
    const { key, timer } = this.pendingPaste;
    clearTimeout(timer);
    this.pendingPaste = null;
    const r = this.rfb;
    if (!r || !this.connected) return;
    if (text && text !== this.lastSent && text !== this.lastRemote) {
      r.clipboardPasteFrom(text);
      this.lastSent = text;
      // RDP : la passerelle retient la touche jusqu'à ce que le texte soit arrivé.
      // VNC : petite attente pour que le serveur ait reçu le texte avant la touche.
      if (this.target.protocol === "vnc") await sleep(250);
    }
    if (r !== this.rfb || !this.connected) return;
    r.sendKey(key[0], key[1], true);
    r.sendKey(key[0], key[1], false);
  }

  onPasteKeyUp(ev) {
    if (this.swallowKeyUp && (ev.code === "KeyV" || ev.code === "Insert" || ev.key === "v" || ev.key === "V")) {
      ev.stopImmediatePropagation();
      this.swallowKeyUp = false;
    }
  }
}

let pasteTarget = null;
document.addEventListener("paste", (ev) => {
  const s = pasteTarget;
  if (!s || !s.pendingPaste) return;
  ev.preventDefault();
  pasteTarget = null;
  s.finishPaste(ev.clipboardData ? ev.clipboardData.getData("text/plain") : null);
}, true);

// ======================================================================
// Onglets et barre d'outils
// ======================================================================

function find(id) { return sessions.find((s) => s.id === Number(id)); }

function activate(s) {
  if (active && active !== s) active.show(false);
  active = s;
  if (!s) return;
  s.show(true);
  showPanel(s.panel);
  renderToolbar();
  s.resize();
  requestAnimationFrame(() => s.focus());
  s.tab.scrollIntoView({ block: "nearest", inline: "nearest" });
}

function renderToolbar() {
  const s = active;
  const sshCount = sessions.filter((t) => t.kind === "ssh").length;
  $("#ses-grid-btn").hidden = sessions.length < 2;
  $("#ses-grid-btn").setAttribute("aria-pressed", String(layout === "grid"));
  $("#ses-sync-btn").hidden = sshCount < 2;
  if (sshCount < 2 && syncOn) setSync(false);
  const ssh = s && s.kind === "ssh";
  const desk = s && s.kind === "desk";
  $("#ses-status").textContent = s ? s.statusText : "";
  $("#ses-sftp-btn").hidden = !(ssh && s.sftpEnabled) || gridActive();
  $("#ses-files-btn").hidden = !(desk && s.filesEnabled) || gridActive();
  $("#ses-keys").hidden = !desk;
  $("#ses-cmds").hidden = !ssh;
  $("#ws-save-btn").hidden = !sessions.length;
  if (!ssh) toggleCmdsMenu(false);
  $("#ses-reconnect").hidden = !(s && s.mode === "off");
  if (!desk) toggleKeysMenu(false);
}

function showPanel(name) {
  const s = active;
  const allowed = {
    sftp: s && s.kind === "ssh" && s.sftpEnabled && !gridActive(),
    files: s && s.kind === "desk" && s.filesEnabled && !gridActive(),
  };
  if (name && !allowed[name]) name = null;
  if (s) s.panel = name;
  $("#sftp-panel").hidden = name !== "sftp";
  $("#desk-files").hidden = name !== "files";
  $("#ses-sftp-btn").setAttribute("aria-pressed", String(name === "sftp"));
  $("#ses-files-btn").setAttribute("aria-pressed", String(name === "files"));
  if (name === "sftp") sftpShow(s);
  if (name === "files") filesShow();
  filesPolling();
  resizeAll();
}

// ---------- mosaïque ----------

function gridColumns(n) {
  if (n <= 1) return 1;
  if (n <= 4) return 2;
  if (n <= 9) return 3;
  return 4;
}

function applyLayout() {
  const grid = layout === "grid" && sessions.length > 1;
  const panes = $("#panes");
  panes.classList.toggle("grid", grid);
  const cols = gridColumns(sessions.length);
  panes.style.setProperty("--cols", String(cols));
  panes.style.setProperty("--rows", String(Math.ceil(sessions.length / cols)));
  // Bureaux : en mosaïque, l'image est mise à l'échelle sans changer la résolution distante.
  for (const t of sessions) if (t.kind === "desk" && t.rfb) t.rfb.resizeSession = !grid;
  if (grid && active && active.panel) showPanel(null); // panneaux Fichiers fermés en mosaïque
  renderToolbar();
  resizeAll();
}

function setLayout(mode) {
  layout = mode;
  try { localStorage.setItem("gna.layout", mode); } catch { /* stockage indisponible */ }
  applyLayout();
  if (active) active.focus();
}

// ---------- saisie synchronisée ----------

function setSync(on) {
  syncOn = on;
  renderSync();
}

function renderSync() {
  const btn = $("#ses-sync-btn");
  btn.setAttribute("aria-pressed", String(syncOn));
  btn.classList.toggle("warn", syncOn);
  const n = sessions.filter((t) => t.kind === "ssh" && t.sync).length;
  btn.textContent = syncOn ? `Synchro active (${n})` : "Saisie synchronisée";
  for (const t of sessions) {
    if (t.kind === "ssh") t.pane.classList.toggle("synced", syncOn && t.sync);
  }
}

function resizeAll() {
  // Le terminal s'adapte à la nouvelle largeur (panneau ouvert ou fermé, fenêtre).
  requestAnimationFrame(() => sessions.forEach((s) => s.resize()));
}

// ---------- bibliothèque de commandes ----------

let snippets = [];
let editingSnippet = null;

async function loadSnippets() {
  try { snippets = await api("/api/snippets"); } catch { /* garde la liste connue */ }
  return snippets;
}

function toggleCmdsMenu(open) {
  const menu = $("#cmds-menu");
  const show = open ?? menu.hidden;
  menu.hidden = !show;
  $("[data-action=cmds-menu]").setAttribute("aria-expanded", String(show));
  if (show) renderCmdsMenu(true);
}

async function renderCmdsMenu(reload) {
  if (reload) await loadSnippets();
  const s = active;
  const n = s && s.kind === "ssh" && syncOn && s.sync ? sessions.filter((t) => t.kind === "ssh" && t.sync).length : 1;
  $("#cmds-target").textContent = n > 1 ? `Envoi dans ${n} terminaux synchronisés` : "Envoi dans le terminal actif";
  $("#cmds-target").classList.toggle("warn-text", n > 1);
  const box = $("#cmds-items");
  box.replaceChildren();
  if (!snippets.length) box.append(el("p", "menu-hint", "Aucune commande enregistrée."));
  for (const sn of snippets) {
    const b = el("button", "cmd-item");
    b.type = "button";
    b.setAttribute("role", "menuitem");
    b.dataset.action = "cmd-send";
    b.dataset.id = String(sn.id);
    b.title = sn.command;
    b.append(el("span", "cmd-name", sn.name), el("span", "cmd-text", sn.command.split("\n")[0] + (sn.run ? "  ⏎" : "")));
    box.append(b);
  }
}

function sendSnippet(id) {
  const sn = snippets.find((x) => x.id === Number(id));
  const s = active;
  if (!sn || !s || s.kind !== "ssh") return;
  // Les retours à la ligne deviennent des validations (Entrée), comme au clavier.
  const text = sn.command.replace(/\r?\n/g, "\r") + (sn.run ? "\r" : "");
  s.input(new TextEncoder().encode(text));
  s.focus();
}

function snippetFormReset(sn) {
  editingSnippet = sn || null;
  const f = $("#snippet-form");
  f.reset();
  f.elements.name.value = sn ? sn.name : "";
  f.elements.command.value = sn ? sn.command : "";
  f.elements.run.checked = sn ? sn.run : true;
  $("#snippet-form-title").textContent = sn ? `Modifier « ${sn.name} »` : "Nouvelle commande";
  $("#snippet-submit").textContent = sn ? "Enregistrer" : "Ajouter";
  $("#snippet-cancel-edit").hidden = !sn;
  const err = f.querySelector("[data-form-error]");
  err.hidden = true;
}

function renderSnippetsList() {
  const ul = $("#snippets-list");
  ul.replaceChildren();
  if (!snippets.length) ul.append(el("li", "empty-line", "Aucune commande pour l'instant."));
  for (const sn of snippets) {
    const li = el("li", "snippet");
    const edit = el("button", "ghost small", "Modifier");
    edit.type = "button";
    edit.dataset.action = "snippet-edit";
    edit.dataset.id = String(sn.id);
    const del = el("button", "danger small", "Supprimer");
    del.type = "button";
    del.dataset.action = "snippet-delete";
    del.dataset.id = String(sn.id);
    li.append(
      el("div", "snippet-main", null),
      el("div", "snippet-actions", null),
    );
    li.firstChild.append(el("strong", "", sn.name), el("code", "", sn.command));
    li.lastChild.append(edit, del);
    ul.append(li);
  }
}

async function openSnippetsDialog() {
  await loadSnippets();
  renderSnippetsList();
  snippetFormReset(null);
  $("#snippets-dialog").showModal();
}

$("#snippet-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const f = ev.target;
  const err = f.querySelector("[data-form-error]");
  const body = JSON.stringify({ name: f.elements.name.value, command: f.elements.command.value, run: f.elements.run.checked });
  try {
    if (editingSnippet) await api(`/api/snippets/${editingSnippet.id}`, { method: "PUT", headers: { "Content-Type": "application/json" }, body });
    else await api("/api/snippets", { method: "POST", headers: { "Content-Type": "application/json" }, body });
    await loadSnippets();
    renderSnippetsList();
    snippetFormReset(null);
    if (!$("#cmds-menu").hidden) renderCmdsMenu(false);
  } catch (e) {
    err.textContent = e.message;
    err.hidden = false;
  }
});

$("#snippets-dialog").addEventListener("click", async (ev) => {
  const btn = ev.target.closest("[data-action]");
  if (!btn) return;
  const sn = snippets.find((x) => x.id === Number(btn.dataset.id));
  switch (btn.dataset.action) {
    case "snippet-edit":
      if (sn) snippetFormReset(sn);
      $("#snippet-form").elements.name.focus();
      break;
    case "snippet-new":
      snippetFormReset(null);
      break;
    case "snippet-delete":
      if (!sn || !confirm(`Supprimer la commande « ${sn.name} » ?`)) return;
      try {
        await api(`/api/snippets/${sn.id}`, { method: "DELETE" }).catch((e) => { if (e.status) throw e; });
      } catch (e) {
        return alert(e.message);
      }
      await loadSnippets();
      renderSnippetsList();
      if (editingSnippet && editingSnippet.id === sn.id) snippetFormReset(null);
      break;
  }
});

// ---------- espaces de travail ----------

async function saveWorkspace() {
  if (!sessions.length) return;
  const existing = await api("/api/workspaces").catch(() => []);
  const name = (prompt("Nom de l'espace de travail :", existing.length ? "" : "Mon espace") || "").trim();
  if (!name) return;
  if (existing.some((w) => w.name.toLowerCase() === name.toLowerCase()) &&
      !confirm(`L'espace « ${name} » existe déjà. Le remplacer par les sessions ouvertes ?`)) return;
  const body = {
    name,
    layout: layout === "grid" ? "grid" : "tabs",
    sync: syncOn,
    items: sessions.map((t) => ({ target_id: t.target.id, sync: t.kind === "ssh" ? t.sync : false })),
  };
  try {
    await api("/api/workspaces", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
    toast(`Espace de travail « ${name} » enregistré : il est disponible dans la liste des machines.`);
    window.dispatchEvent(new Event("gna-workspaces-changed"));
  } catch (e) {
    toast(e.message);
  }
}

function toggleKeysMenu(open) {
  const menu = $("#desk-keys-menu");
  const show = open ?? menu.hidden;
  menu.hidden = !show;
  $("[data-action=desk-keys]").setAttribute("aria-expanded", String(show));
  if (show) menu.querySelector("button").focus();
}

function closeSession(s) {
  const i = sessions.indexOf(s);
  if (i < 0) return;
  sessions.splice(i, 1);
  s.destroy();
  if (active === s) {
    active = null;
    const next = sessions[Math.min(i, sessions.length - 1)];
    if (next) activate(next);
  }
  ctx.onChange(sessions.length);
  applyLayout();
  renderSync();
  if (!sessions.length) {
    showPanel(null);
    if (document.fullscreenElement) document.exitFullscreen().catch(() => {});
    ctx.onHome();
  }
}

// ======================================================================
// Fichiers SSH (SFTP) : panneau de la session active
// ======================================================================

const sftpMsg = (t) => { $("#sftp-msg").textContent = t || ""; };

function sftpShow(s) {
  $("#sftp-list").replaceChildren();
  $("#sftp-path").value = s.sftp.cwd || "";
  sftpMsg("");
  if (s.mode === "on") sftpList(s, s.sftp.cwd);
  else sftpMsg("Connexion…");
}

async function sftpList(s, dir) {
  if (!s.sftp.tid) return;
  sftpMsg("");
  try {
    const q = dir ? `?path=${encodeURIComponent(dir)}` : "";
    const data = await api(`/api/sftp/${s.sftp.tid}/list${q}`);
    s.sftp.cwd = data.path;
    if (s !== active || s.panel !== "sftp") return;
    $("#sftp-path").value = data.path;
    sftpRender(s, data.entries);
    if (data.truncated) sftpMsg("Dossier très volumineux : seuls les premiers éléments sont affichés.");
  } catch (e) {
    if (s === active) sftpMsg(e.message);
  }
}

function sftpRender(s, entries) {
  const ul = $("#sftp-list");
  ul.replaceChildren();
  if (!entries.length) {
    ul.append(el("li", "empty", "Dossier vide."));
    return;
  }
  const cwd = s.sftp.cwd;
  for (const e of entries) {
    const li = document.createElement("li");
    const full = (cwd === "/" ? "" : cwd) + "/" + e.name;
    if (e.dir) {
      const b = el("button", "entry dir", e.name + "/");
      b.type = "button";
      b.addEventListener("click", () => sftpList(s, full));
      li.append(b, el("span", "meta", e.link ? "lien vers un dossier" : "dossier"));
    } else {
      const a = el("a", "", "Télécharger");
      a.href = `/api/sftp/${s.sftp.tid}/file?path=${encodeURIComponent(full)}`;
      a.setAttribute("download", e.name);
      li.append(el("span", "name", e.name), el("span", "meta", `${fmtSize(e.size)} · ${new Date(e.modified).toLocaleString()}`), a);
    }
    ul.append(li);
  }
}

async function sftpUpload(s, files) {
  if (!s.sftp.tid || !files.length) return;
  if (s.panel !== "sftp") showPanel("sftp");
  if (!s.sftp.cwd) await sftpList(s, "");
  const bar = $("#sftp-progress");
  bar.hidden = false;
  let ok = 0;
  const send = (f, overwrite) => uploadXHR(
    `/api/sftp/${s.sftp.tid}/file?${new URLSearchParams({ dir: s.sftp.cwd, name: f.name, overwrite: overwrite ? "1" : "0" })}`, f, bar);
  for (const f of files) {
    try {
      await send(f, false);
      ok++;
    } catch (e) {
      if (e.status === 409 && confirm(`« ${f.name} » existe déjà dans ${s.sftp.cwd}. Le remplacer ?`)) {
        try { await send(f, true); ok++; } catch (e2) { sftpMsg(`${f.name} : ${e2.message}`); }
      } else if (e.status !== 409) {
        sftpMsg(`${f.name} : ${e.message}`);
      }
    }
  }
  bar.hidden = true;
  await sftpList(s, s.sftp.cwd);
  if (ok) sftpMsg(ok === 1 ? "Fichier envoyé." : `${ok} fichiers envoyés.`);
}

// ======================================================================
// Fichiers RDP (lecteur partagé « Gna ») : panneau de la session active
// ======================================================================

const filesMsg = (t) => { $("#files-msg").textContent = t || ""; };
let pollTimer = null;

function filesShow() {
  $("#files-list").replaceChildren();
  $("#files-usage").textContent = "";
  filesMsg("");
  filesRefresh();
}

async function filesRefresh() {
  const s = active;
  if (!s || s.kind !== "desk" || !s.transferId || s.panel !== "files") return;
  const tid = s.transferId;
  try {
    const data = await api(`/api/transfers/${tid}`);
    if (s !== active || s.transferId !== tid) return;
    const list = $("#files-list");
    list.replaceChildren();
    if (!data.files.length) list.append(el("li", "empty", "Aucun fichier pour l'instant."));
    for (const f of data.files) {
      const a = el("a", "", "Télécharger");
      a.href = `/api/transfers/${tid}/files?path=${encodeURIComponent(f.path)}`;
      a.setAttribute("download", f.path.split("/").pop());
      const li = document.createElement("li");
      li.append(el("span", "name", f.path), el("span", "meta", `${fmtSize(f.size)} · ${new Date(f.modified).toLocaleTimeString()}`), a);
      list.append(li);
    }
    $("#files-usage").textContent = `Espace utilisé : ${fmtSize(data.used)} sur ${fmtSize(data.max)}`;
  } catch (e) {
    if (s === active && s.mode === "on") filesMsg(e.message);
  }
}

function filesPolling() {
  clearInterval(pollTimer);
  pollTimer = null;
  if (active && active.panel === "files") pollTimer = setInterval(filesRefresh, 3000);
}

async function filesUpload(s, files) {
  if (!s.transferId || !files.length) return;
  if (s.panel !== "files") showPanel("files");
  const bar = $("#files-progress");
  bar.hidden = false;
  let ok = 0;
  for (const f of files) {
    try {
      await uploadXHR(`/api/transfers/${s.transferId}/files?name=${encodeURIComponent(f.name)}`, f, bar);
      ok++;
    } catch (e) {
      filesMsg(`${f.name} : ${e.message}`);
    }
  }
  bar.hidden = true;
  if (ok) filesMsg(ok === 1 ? "Fichier envoyé." : `${ok} fichiers envoyés.`);
  filesRefresh();
}

// ======================================================================
// Événements
// ======================================================================

document.addEventListener("click", (ev) => {
  if (!ev.target.closest(".menu")) {
    toggleKeysMenu(false);
    toggleCmdsMenu(false);
  }
  const btn = ev.target.closest("[data-action]");
  if (!btn || !btn.closest("#session-view")) return;
  const s = active;
  switch (btn.dataset.action) {
    case "ses-switch":
      activate(find(btn.dataset.session));
      break;
    case "ses-tab-close":
      closeSession(find(btn.dataset.session));
      break;
    case "ses-close":
      if (s) closeSession(s);
      break;
    case "ses-home":
      if (document.fullscreenElement) document.exitFullscreen().catch(() => {});
      ctx.onHome();
      break;
    case "ses-reconnect":
      if (s) s.reconnect();
      break;
    case "ses-fullscreen":
      if (document.fullscreenElement) document.exitFullscreen();
      else $("#session-view").requestFullscreen().catch(() => {});
      break;
    case "cmds-menu":
      toggleCmdsMenu();
      break;
    case "cmd-send":
      toggleCmdsMenu(false);
      sendSnippet(btn.dataset.id);
      break;
    case "cmds-manage":
      toggleCmdsMenu(false);
      openSnippetsDialog();
      break;
    case "ws-save":
      saveWorkspace();
      break;
    case "ses-grid":
      setLayout(layout === "grid" ? "tabs" : "grid");
      break;
    case "ses-sync":
      setSync(!syncOn);
      if (active) active.focus();
      break;
    case "ses-panel":
      showPanel(s && s.panel === btn.dataset.panel ? null : btn.dataset.panel);
      break;
    case "desk-keys":
      toggleKeysMenu();
      break;
    case "desk-key":
      toggleKeysMenu(false);
      if (s && s.kind === "desk") s.sendCombo(btn.dataset.combo);
      break;
    case "sftp-refresh":
      if (s && s.kind === "ssh") sftpList(s, s.sftp.cwd);
      break;
    case "sftp-up":
      if (s && s.kind === "ssh" && s.sftp.cwd && s.sftp.cwd !== "/") sftpList(s, s.sftp.cwd.replace(/\/[^/]*$/, "") || "/");
      break;
    case "sftp-mkdir": {
      if (!s || s.kind !== "ssh" || !s.sftp.tid) return;
      const name = prompt("Nom du nouveau dossier :");
      if (!name) return;
      api(`/api/sftp/${s.sftp.tid}/mkdir`, {
        method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ dir: s.sftp.cwd, name }),
      }).then(() => sftpList(s, s.sftp.cwd), (e) => sftpMsg(e.message));
      break;
    }
    case "files-refresh":
      filesRefresh();
      break;
  }
});

document.addEventListener("keydown", (ev) => {
  if (ev.key === "Escape" && !$("#desk-keys-menu").hidden) {
    toggleKeysMenu(false);
    $("[data-action=desk-keys]").focus();
  }
  if (ev.key === "Escape" && !$("#cmds-menu").hidden) {
    toggleCmdsMenu(false);
    $("[data-action=cmds-menu]").focus();
  }
});

document.addEventListener("submit", (ev) => {
  if (ev.target.id !== "sftp-path-form") return;
  ev.preventDefault();
  if (active && active.kind === "ssh") sftpList(active, $("#sftp-path").value.trim() || "/");
});

document.addEventListener("change", (ev) => {
  const s = active;
  if (!s) return;
  if (ev.target.id === "sftp-input" || ev.target.id === "files-input") {
    const files = Array.from(ev.target.files);
    ev.target.value = "";
    if (ev.target.id === "sftp-input" && s.kind === "ssh") sftpUpload(s, files);
    if (ev.target.id === "files-input" && s.kind === "desk") filesUpload(s, files);
  }
});

window.addEventListener("resize", resizeAll);

// Glisser-déposer de fichiers sur la session active.
let dragDepth = 0;
const dropTarget = () => {
  const s = active;
  if (!s || gridActive()) return null;
  if (s.kind === "ssh" && s.sftp.tid) return "Déposez les fichiers pour les envoyer dans le dossier affiché (SFTP)";
  if (s.kind === "desk" && s.transferId) return "Déposez les fichiers pour les envoyer dans le lecteur « Gna »";
  return null;
};
const isFileDrag = (ev) => ev.dataTransfer && Array.from(ev.dataTransfer.types).includes("Files") && dropTarget();
const view = $("#session-view");
view.addEventListener("dragenter", (ev) => {
  if (!isFileDrag(ev)) return;
  ev.preventDefault();
  dragDepth++;
  $("#drop-hint-text").textContent = dropTarget();
  $("#drop-hint").hidden = false;
});
view.addEventListener("dragover", (ev) => { if (isFileDrag(ev)) ev.preventDefault(); });
view.addEventListener("dragleave", () => { if (dragDepth > 0 && --dragDepth === 0) $("#drop-hint").hidden = true; });
view.addEventListener("drop", (ev) => {
  if (!isFileDrag(ev)) return;
  ev.preventDefault();
  dragDepth = 0;
  $("#drop-hint").hidden = true;
  const files = Array.from(ev.dataTransfer.files);
  if (active.kind === "ssh") sftpUpload(active, files);
  else filesUpload(active, files);
});

// ======================================================================
// Interface avec app.js
// ======================================================================

window.GnaSessions = {
  // Ouvre une nouvelle session dans un onglet (l'écran des sessions doit être affiché).
  open(target) {
    const s = target.protocol === "ssh" ? new SshSession(target) : new DeskSession(target);
    sessions.push(s);
    activate(s);
    applyLayout();
    renderSync();
    ctx.onChange(sessions.length);
    return s;
  },
  // Revient sur l'écran des sessions (après un passage par la liste des machines).
  resume() {
    if (!active && sessions.length) activate(sessions[sessions.length - 1]);
    else if (active) activate(active);
  },
  count() { return sessions.length; },
  // Ouvre un espace de travail : ses sessions, leur disposition et la synchro.
  openWorkspace(ws, entries) {
    let first = null;
    for (const { target, sync } of entries) {
      const s = target.protocol === "ssh" ? new SshSession(target) : new DeskSession(target);
      if (s.kind === "ssh") {
        s.sync = sync !== false;
        s.syncBox.checked = s.sync;
      }
      sessions.push(s);
      if (!first) first = s;
    }
    layout = ws.layout === "grid" ? "grid" : "tabs";
    activate(first);
    applyLayout();
    setSync(Boolean(ws.sync) && sessions.filter((t) => t.kind === "ssh").length > 1);
    ctx.onChange(sessions.length);
  },
  closeAll() {
    for (const s of sessions.slice()) {
      sessions.splice(sessions.indexOf(s), 1);
      s.destroy();
    }
    active = null;
    syncOn = false;
    showPanel(null);
    applyLayout();
    ctx.onChange(0);
  },
};
window.dispatchEvent(new Event("gna-sessions-ready"));
