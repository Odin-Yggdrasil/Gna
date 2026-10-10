"use strict";

// Écran local de l'application : choix du serveur, validation de l'empreinte du
// certificat, puis bascule de la fenêtre vers l'interface de Gná via le relais local.

const invoke = window.__TAURI__.core.invoke;
const $ = (s) => document.querySelector(s);
const views = ["address", "verify", "changed", "saved", "busy"];
let pending = null; // { address, fingerprint } en attente de validation
let saved = null;

function show(name) {
  for (const v of views) $(`#view-${v}`).hidden = v !== name;
  const input = $(`#view-${name} input`);
  if (input) input.focus();
}

function setError(msg) {
  $("#error").textContent = msg || "";
  $("#error").hidden = !msg;
}

// Empreinte lisible : groupes de 8 octets par ligne.
const pretty = (fp) => fp.split(":").reduce((acc, b, i) => acc + (i && i % 8 === 0 ? "\n" : i ? ":" : "") + b, "");

function setFp(el, fp) {
  el.textContent = pretty(fp);
  el.style.whiteSpace = "pre";
}

async function connect(address, fingerprint) {
  setError("");
  $("#busy-text").textContent = `Connexion à ${address}…`;
  show("busy");
  try {
    const r = await invoke("connect_server", { address, fingerprint });
    if (r.status === "ready") {
      window.location.replace(r.url);
      return;
    }
    if (r.status === "cert_changed") {
      pending = { address, fingerprint: r.fingerprint };
      setFp($("#changed-old"), fingerprint);
      setFp($("#changed-new"), r.fingerprint);
      show("changed");
      return;
    }
    setError(r.message);
  } catch (e) {
    setError(String(e));
  }
  saved ? showSaved() : show("address");
}

function showSaved() {
  $("#saved-address").textContent = saved.address;
  setFp($("#saved-fp"), saved.fingerprint);
  show("saved");
}

$("#view-address").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  setError("");
  const btn = ev.target.querySelector("button");
  btn.disabled = true;
  try {
    const r = await invoke("probe_server", { address: ev.target.elements.address.value });
    pending = { address: r.address, fingerprint: r.fingerprint };
    $("#verify-server").textContent = r.address;
    setFp($("#verify-fp"), r.fingerprint);
    show("verify");
  } catch (e) {
    setError(String(e));
  } finally {
    btn.disabled = false;
  }
});

document.addEventListener("click", async (ev) => {
  const a = ev.target.closest("[data-action]");
  if (!a) return;
  switch (a.dataset.action) {
    case "trust":
    case "accept-new":
      return connect(pending.address, pending.fingerprint);
    case "connect":
      return connect(saved.address, saved.fingerprint);
    case "change":
      setError("");
      if (saved) $("#view-address").elements.address.value = saved.address.replace(/^https:\/\//, "");
      return show("address");
    case "settings":
      setError("");
      return saved ? showSaved() : show("address");
  }
});

(async () => {
  saved = await invoke("saved_server");
  const settings = new URLSearchParams(location.search).has("settings");
  if (!saved) return show("address");
  if (settings) return showSaved();
  connect(saved.address, saved.fingerprint); // lancement normal : connexion directe
})();
