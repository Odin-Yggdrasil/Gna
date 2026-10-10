// Relecture des sessions VNC et RDP enregistrées, avec noVNC (module ES).
// (Les sessions en direct sont gérées par sessions.js.)
import RFB from "/vendor/novnc/core/rfb.js";

const $ = (s) => document.querySelector(s);

// ======================================================================
// Relecture d'une session VNC enregistrée
//
// Le flux RFB enregistré est réinjecté dans noVNC par un canal factice qui imite
// une WebSocket : le décodage est identique à la session en direct. noVNC ne sait
// pas revenir en arrière ; pour reculer, on recrée le lecteur et on rejoue
// instantanément le flux jusqu'au point voulu.
// ======================================================================

const IDLE_GAP = 3000; // ms : au-delà, une inactivité est raccourcie (option « sauter »)

class ReplayChannel {
  constructor(onReady) {
    // Propriétés exigées par noVNC (Websock.attach)
    this.binaryType = "arraybuffer";
    this.protocol = "binary";
    this.readyState = WebSocket.CONNECTING;
    this.onopen = null;
    this.onmessage = null;
    this.onerror = null;
    this.onclose = null;
    this._sends = 0;
    this._onReady = onReady;
  }
  // Poignée de main RFB 3.8 jouée côté « serveur » : version → sécurité « aucune » → OK.
  send() {
    this._sends++;
    if (this._sends === 1) this._emit(new Uint8Array([1, 1]));
    else if (this._sends === 2) this._emit(new Uint8Array([0, 0, 0, 0]));
    else if (this._sends === 3) setTimeout(() => this._onReady(), 0); // ClientInit reçu
    // Les messages suivants (demandes de rafraîchissement…) sont ignorés.
  }
  close() {
    if (this.readyState === WebSocket.CLOSED) return;
    this.readyState = WebSocket.CLOSED;
    if (this.onclose) this.onclose({ code: 1000, reason: "", wasClean: true });
  }
  start() {
    this.readyState = WebSocket.OPEN;
    if (this.onopen) this.onopen();
    this._emit(new TextEncoder().encode("RFB 003.008\n"));
  }
  _emit(bytes) {
    setTimeout(() => this.push(bytes), 0);
  }
  push(bytes) {
    if (this.readyState === WebSocket.OPEN && this.onmessage) {
      this.onmessage({ data: bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength) });
    }
  }
}

function parseRecording(buf) {
  const u8 = new Uint8Array(buf);
  const magic = "GNAVNC1\n";
  if (new TextDecoder().decode(u8.subarray(0, magic.length)) !== magic) throw new Error("enregistrement VNC invalide");
  let p = magic.length;
  const nl = u8.indexOf(10, p);
  const header = JSON.parse(new TextDecoder().decode(u8.subarray(p, nl)));
  p = nl + 1;
  const dv = new DataView(buf);
  const frames = [];
  while (p + 8 <= u8.length) {
    const at = dv.getUint32(p);
    const len = dv.getUint32(p + 4);
    if (p + 8 + len > u8.length) break; // fin coupée (arrêt brutal)
    frames.push({ at, data: u8.subarray(p + 8, p + 8 + len) });
    p += 8 + len;
  }
  return { header, frames };
}

// Échelle de temps « compressée » : chaque inactivité de plus de IDLE_GAP est ramenée à IDLE_GAP.
function buildTimeline(frames, skipIdle) {
  let shift = 0;
  let prev = 0;
  for (const f of frames) {
    const gap = f.at - prev;
    if (skipIdle && gap > IDLE_GAP) shift += gap - IDLE_GAP;
    f.t = f.at - shift;
    prev = f.at;
  }
  return frames.length ? frames[frames.length - 1].t : 0;
}

const fmt = (ms) => {
  const s = Math.floor(ms / 1000);
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), sec = s % 60;
  return (h ? `${h}:${String(m).padStart(2, "0")}` : String(m)) + ":" + String(sec).padStart(2, "0");
};

const vr = {
  frames: [], duration: 0, rfb: null, chan: null, next: 0, pos: 0,
  playing: false, speed: 1, wallStart: 0, posStart: 0, timer: null, ready: false, gen: 0,
};

function vrUI() {
  $("#vnc-time").textContent = `${fmt(vr.pos)} / ${fmt(vr.duration)}`;
  if (document.activeElement !== $("#vnc-seek")) $("#vnc-seek").value = String(vr.pos / 1000);
  $("[data-action=vnc-play]").textContent = vr.playing ? "⏸" : "▶";
}

function vrDestroy() {
  clearTimeout(vr.timer);
  vr.gen++;
  if (vr.rfb) {
    try { vr.chan.close(); } catch { /* déjà fermé */ }
    vr.rfb = null;
  }
  vr.chan = null;
  vr.ready = false;
  $("#vnc-screen").replaceChildren();
}

// Crée un lecteur neuf, puis rejoue instantanément le flux jusqu'à `pos`.
function vrRebuild(pos) {
  vrDestroy();
  const gen = vr.gen;
  vr.next = 0;
  vr.pos = 0;
  vr.chan = new ReplayChannel(() => {
    if (gen !== vr.gen) return;
    vr.ready = true;
    vrFeedUntil(pos);
    if (vr.playing) vrSchedule();
    vrUI();
  });
  vr.rfb = new RFB($("#vnc-screen"), vr.chan, {});
  vr.rfb.viewOnly = true;
  vr.rfb.scaleViewport = true;
  vr.rfb.background = getComputedStyle(document.documentElement).getPropertyValue("--term-bg").trim() || "#0b0e13";
  vr.chan.start();
}

function vrFeedUntil(pos) {
  while (vr.next < vr.frames.length && vr.frames[vr.next].t <= pos) {
    vr.chan.push(vr.frames[vr.next].data);
    vr.next++;
  }
  vr.pos = Math.min(pos, vr.duration);
}

function vrSchedule() {
  clearTimeout(vr.timer);
  vr.wallStart = performance.now();
  vr.posStart = vr.pos;
  const gen = vr.gen;
  const tick = () => {
    if (gen !== vr.gen || !vr.playing) return;
    const pos = vr.posStart + (performance.now() - vr.wallStart) * vr.speed;
    vrFeedUntil(pos);
    vrUI();
    if (vr.next >= vr.frames.length && pos >= vr.duration) {
      vr.playing = false;
      vrUI();
      return;
    }
    vr.timer = setTimeout(tick, 30);
  };
  tick();
}

function vrSeek(pos) {
  pos = Math.max(0, Math.min(pos, vr.duration));
  if (!vr.ready) return;
  if (pos >= vr.pos) {
    vrFeedUntil(pos); // en avant : on continue sur le même lecteur
    if (vr.playing) vrSchedule();
    vrUI();
  } else {
    vrRebuild(pos); // en arrière : on repart du début
  }
}

window.GnaVncReplay = {
  async open(session) {
    $("#vnc-loading").hidden = false;
    $("#vnc-loading").textContent = "Chargement de l'enregistrement…";
    vr.playing = true;
    vr.speed = Number($("#vnc-speed").value);
    try {
      const res = await fetch(`/api/admin/recordings/${session.id}/vnc`, { credentials: "same-origin" });
      if (!res.ok) throw new Error((await res.json().catch(() => ({}))).error || `Erreur ${res.status}`);
      const { frames } = parseRecording(await res.arrayBuffer());
      if (!frames.length) throw new Error("enregistrement vide");
      vr.frames = frames;
      vr.duration = buildTimeline(frames, $("#vnc-skip").checked);
      $("#vnc-seek").max = String(vr.duration / 1000);
      $("#vnc-loading").hidden = true;
      vrRebuild(0);
      vrUI();
    } catch (e) {
      $("#vnc-loading").textContent = "Impossible de lire l'enregistrement : " + e.message;
    }
  },
  close() {
    vr.playing = false;
    vrDestroy();
    vr.frames = [];
  },
};

document.addEventListener("click", (ev) => {
  if (!ev.target.closest("[data-action=vnc-play]") || !vr.ready) return;
  if (!vr.playing && vr.pos >= vr.duration) {
    vr.playing = true;
    return vrRebuild(0); // relancer depuis le début
  }
  vr.playing = !vr.playing;
  if (vr.playing) vrSchedule();
  else clearTimeout(vr.timer);
  vrUI();
});
document.addEventListener("input", (ev) => {
  if (ev.target.id === "vnc-seek") $("#vnc-time").textContent = `${fmt(Number(ev.target.value) * 1000)} / ${fmt(vr.duration)}`;
});
document.addEventListener("change", (ev) => {
  if (ev.target.id === "vnc-seek") vrSeek(Number(ev.target.value) * 1000);
  if (ev.target.id === "vnc-speed") {
    vr.speed = Number(ev.target.value);
    if (vr.playing) vrSchedule();
  }
  if (ev.target.id === "vnc-skip" && vr.frames.length) {
    // Garder le même instant réel en changeant d'échelle de temps.
    const realAt = vr.next > 0 ? vr.frames[vr.next - 1].at : 0;
    vr.duration = buildTimeline(vr.frames, ev.target.checked);
    $("#vnc-seek").max = String(vr.duration / 1000);
    const f = vr.frames.find((x) => x.at >= realAt);
    vr.pos = f ? f.t : vr.duration;
    if (vr.playing) vrSchedule();
    vrUI();
  }
});
