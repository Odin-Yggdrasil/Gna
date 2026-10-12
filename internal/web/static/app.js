"use strict";

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];
const state = { me: null, targets: [], users: [], groups: [], tab: "targets", editing: null, editingGroup: null };

// ---------- API ----------

async function api(method, path, body) {
  const res = await fetch(path, {
    method,
    credentials: "same-origin",
    headers: body !== undefined ? { "Content-Type": "application/json" } : {},
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  if (res.status === 204) return null;
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    const err = new Error(data.error || `Erreur ${res.status}`);
    err.status = res.status;
    throw err;
  }
  return data;
}

// Petit utilitaire de création d'éléments : le texte passe toujours par textContent.
function el(tag, props = {}, ...children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (k === "class") e.className = v;
    else if (k === "text") e.textContent = v;
    else if (k.startsWith("data-") || k.startsWith("aria-")) e.setAttribute(k, v);
    else e[k] = v;
  }
  for (const c of children) if (c != null) e.append(c);
  return e;
}

function setError(msg) {
  for (const e of $$("[data-error]")) {
    e.textContent = msg || "";
    e.hidden = !msg;
  }
}

const fmtDate = (s) => new Date(s).toLocaleString("fr-FR");
function fmtDuration(a, b) {
  if (!b) return "en cours";
  const s = Math.round((new Date(b) - new Date(a)) / 1000);
  if (s < 60) return `${s} s`;
  if (s < 3600) return `${Math.floor(s / 60)} min ${s % 60} s`;
  return `${Math.floor(s / 3600)} h ${Math.floor((s % 3600) / 60)} min`;
}

// ---------- navigation ----------

function showScreen(name) {
  $("#auth").hidden = name !== "auth";
  $("#app").hidden = name !== "app";
  $("#session-view").hidden = name !== "sessions";
  $("#replay-view").hidden = name !== "replay";
}

function showAuthView(view) {
  showScreen("auth");
  for (const v of ["login", "mfa", "lock", "enroll"]) $(`#view-${v}`).hidden = v !== view;
  const first = $(`#view-${view} input:not([hidden])`);
  if (first) first.focus();
}

async function route() {
  setError("");
  let me;
  try {
    me = await api("GET", "/api/me");
  } catch {
    state.me = null;
    return showAuthView("login");
  }
  if (me.stage !== "full") state.me = null;
  if (me.stage === "mfa") return showAuthView("mfa");
  if (me.stage === "enroll") return startEnroll();
  if (me.stage === "locked") {
    $("#lock-user").textContent = me.username;
    $("#view-lock").elements.username.value = me.username;
    return showAuthView("lock");
  }
  state.me = me;
  applyTheme((me.prefs && me.prefs.theme) || "system");
  $("#me-name").textContent = me.username;
  $("#me-admin").hidden = !me.is_admin;
  for (const e of $$("[data-admin]")) e.hidden = !me.is_admin;
  showScreen("app");
  selectTab(me.is_admin ? state.tab : "targets");
}

function selectTab(tab) {
  state.tab = tab;
  for (const b of $$("[data-tab]")) b.setAttribute("aria-selected", String(b.dataset.tab === tab));
  for (const p of $$("[data-panel]")) p.hidden = p.dataset.panel !== tab;
  setError("");
  const loaders = { targets: loadTargets, users: loadUsers, groups: loadGroups, sessions: loadSessions, audit: loadAudit };
  loaders[tab]().catch(handleAppError);
}

function handleAppError(e) {
  if (e.status === 401) return route();
  setError(e.message);
}

// ---------- authentification ----------

async function startEnroll() {
  showAuthView("enroll");
  const data = await api("POST", "/api/auth/totp/enroll", {});
  $("#qr").src = data.qr;
  $("#secret").textContent = data.secret.replace(/(.{4})/g, "$1 ").trim();
}

function onSubmit(form, handler) {
  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    const btn = form.querySelector("button[type=submit]");
    btn.disabled = true;
    setError("");
    try {
      await handler(new FormData(form));
      form.reset();
      await route();
    } catch (e) {
      setError(e.message);
    } finally {
      btn.disabled = false;
    }
  });
}

onSubmit($("#view-login"), (f) =>
  api("POST", "/api/auth/login", { username: f.get("username"), password: f.get("password") }));
onSubmit($("#view-mfa"), (f) => api("POST", "/api/auth/totp/verify", { code: f.get("code") }));
onSubmit($("#view-lock"), (f) => api("POST", "/api/auth/unlock", { password: f.get("password") }));

// ---------- inactivité ----------
// Toutes les 30 s, l'interface demande au serveur l'état de la session en signalant s'il y a
// eu une action réelle (clavier, souris, toucher) depuis la vérification précédente : seule
// l'activité repousse le verrouillage. Si la session a été verrouillée ou fermée côté
// serveur, l'écran correspondant s'affiche aussitôt.
const activity = { last: Date.now(), sent: 0 };
for (const type of ["keydown", "pointerdown", "pointermove", "wheel", "touchstart"]) {
  window.addEventListener(type, () => { activity.last = Date.now(); }, { capture: true, passive: true });
}
async function pingSession() {
  if (!state.me) return;
  const active = activity.last > activity.sent;
  activity.sent = Date.now();
  try {
    const r = await api("POST", "/api/auth/ping", { active });
    if (r.stage !== "full") route();
  } catch (e) {
    if (e.status === 401) route();
  }
}
setInterval(pingSession, 30000);
document.addEventListener("visibilitychange", () => { if (!document.hidden) pingSession(); });
onSubmit($("#view-enroll"), (f) => api("POST", "/api/auth/totp/enroll/confirm", { code: f.get("code") }));

// ---------- cibles ----------

// ---------- machines : favoris, dossiers, étiquettes, recherche ----------

const org = { search: "", collapsed: new Set() };
try { org.collapsed = new Set(JSON.parse(localStorage.getItem("gna.collapsed") || "[]")); } catch { /* stockage indisponible */ }
const saveCollapsed = () => { try { localStorage.setItem("gna.collapsed", JSON.stringify([...org.collapsed])); } catch { /* idem */ } };

async function loadTargets() {
  const [targets, workspaces] = await Promise.all([api("GET", "/api/targets"), api("GET", "/api/workspaces")]);
  state.targets = targets;
  state.workspaces = workspaces;
  $("#targets-empty").hidden = targets.length > 0;
  $("#targets-empty").textContent = state.me.is_admin
    ? "Aucune cible pour l'instant. Ajoutez votre première machine."
    : "Aucune cible ne vous est attribuée pour l'instant. Contactez un administrateur.";
  $("#targets-tools").hidden = targets.length < 2;
  state.folders = [...new Set(targets.map((t) => t.folder).filter(Boolean))].sort((x, y) => x.localeCompare(y, "fr"));
  renderWorkspaces();
  renderTargets();
}

function targetMatches(t) {
  const q = org.search.trim().toLowerCase();
  if (!q) return true;
  const hay = [t.name, t.host, t.username, t.folder, t.description, t.protocol].join(" ").toLowerCase();
  return q.split(/\s+/).every((w) => hay.includes(w));
}

function targetCard(t) {
  const isVNC = t.protocol === "vnc"; // bureau sans identifiant
  const isDesktop = t.protocol === "vnc" || t.protocol === "rdp";
  const actions = el("div", { class: "target-actions" },
    el("button", { class: "open", type: "button", text: isDesktop ? "Ouvrir le bureau" : "Ouvrir le terminal",
      "data-action": "open", "data-id": t.id }));
  if (state.me.is_admin) {
    actions.append(
      el("button", { class: "ghost", type: "button", text: "Modifier", "data-action": "edit", "data-id": t.id }),
      el("button", { class: "danger", type: "button", text: "Supprimer", "data-action": "delete", "data-id": t.id }));
  }
  const star = el("button", {
    type: "button", class: "star" + (t.favorite ? " on" : ""), text: t.favorite ? "★" : "☆",
    "data-action": "favorite", "data-id": t.id, "aria-pressed": String(Boolean(t.favorite)),
    title: t.favorite ? "Retirer des favoris" : "Ajouter aux favoris", "aria-label": t.favorite ? "Retirer des favoris" : "Ajouter aux favoris",
  });
  return el("li", { class: "target" },
    el("div", { class: "target-head" },
      el("span", { class: "proto", text: t.protocol }),
      el("strong", { text: t.name }),
      star),
    el("div", { class: "target-addr", text: isVNC ? `${t.host}:${t.port}` : `${t.username}@${t.host}:${t.port}` }),
    t.description ? el("div", { class: "target-desc", text: t.description }) : null,
    isVNC ? null : el("div", { class: "target-fp", text: t.host_key_fingerprint || "clé d'hôte non enregistrée" }),
    t.access ? el("div", { class: "group-meta", text: accessSummary(t.access) }) : null,
    actions);
}

// Groupes : Favoris, puis un groupe par dossier, puis « Sans dossier ».
function renderTargets() {
  const box = $("#targets");
  box.replaceChildren();
  const shown = state.targets.filter(targetMatches);
  $("#targets-nomatch").hidden = !(state.targets.length && !shown.length);
  const groups = [];
  const favs = shown.filter((t) => t.favorite);
  if (favs.length) groups.push({ key: "★", title: "★ Favoris", items: favs });
  const folders = [...new Set(shown.map((t) => t.folder).filter(Boolean))].sort((a, b) => a.localeCompare(b, "fr"));
  for (const f of folders) groups.push({ key: "d:" + f, title: f, items: shown.filter((t) => t.folder === f) });
  const loose = shown.filter((t) => !t.folder);
  if (loose.length) groups.push({ key: "-", title: folders.length ? "Sans dossier" : "", items: loose });
  const searching = Boolean(org.search.trim());
  for (const g of groups) {
    const collapsed = !searching && org.collapsed.has(g.key);
    const ul = el("ul", { class: "targets" }, ...g.items.map(targetCard));
    ul.hidden = collapsed;
    const section = el("section", { class: "tgroup" });
    if (g.title) {
      section.append(el("button", {
        type: "button", class: "tgroup-head", "data-action": "toggle-group", "data-group": g.key,
        "aria-expanded": String(!collapsed), text: `${collapsed ? "▸" : "▾"} ${g.title} (${g.items.length})`,
      }));
    }
    section.append(ul);
    box.append(section);
  }
}

// ---------- espaces de travail ----------

function renderWorkspaces() {
  const list = $("#workspaces");
  const ws = state.workspaces || [];
  $("#workspaces-box").hidden = !state.targets.length;
  $("#workspaces-empty").hidden = ws.length > 0;
  const byId = new Map(state.targets.map((t) => [t.id, t]));
  list.replaceChildren(...ws.map((w) => {
    const names = w.items.map((it) => (byId.get(it.target_id) || {}).name).filter(Boolean);
    const meta = [`${w.items.length} session${w.items.length > 1 ? "s" : ""}`, w.layout === "grid" ? "mosaïque" : "onglets"];
    if (w.sync) meta.push("saisie synchronisée");
    return el("li", { class: "ws" },
      el("div", { class: "ws-main" },
        el("strong", { text: w.name }),
        el("span", { class: "ws-meta", text: meta.join(" · ") }),
        el("span", { class: "ws-targets", text: names.join(", ") || "machines indisponibles" })),
      el("div", { class: "ws-actions" },
        el("button", { type: "button", text: "Ouvrir", "data-action": "ws-open", "data-id": w.id }),
        el("button", { type: "button", class: "ghost", text: "Modifier", "data-action": "ws-edit", "data-id": w.id }),
        el("button", { type: "button", class: "ghost", text: "Supprimer", "data-action": "ws-delete", "data-id": w.id })));
  }));
}

// Création / modification d'un espace sans ouvrir de connexion.
let editingWorkspace = null;
const wsForm = $("#ws-form");

function openWorkspaceForm(w) {
  editingWorkspace = w || null;
  wsForm.reset();
  const err = wsForm.querySelector("[data-form-error]");
  err.hidden = true;
  $("#ws-form-title").textContent = w ? `Modifier « ${w.name} »` : "Nouvel espace de travail";
  wsForm.elements.name.value = w ? w.name : "";
  wsForm.elements.layout.value = w ? w.layout : "tabs";
  wsForm.elements.sync.checked = w ? w.sync : false;
  const chosen = new Set(w ? w.items.map((it) => it.target_id) : []);
  // Machines rangées par dossier, comme dans la liste.
  const sorted = [...state.targets].sort((a, b) =>
    (a.folder || "\uffff").localeCompare(b.folder || "\uffff", "fr") || a.name.localeCompare(b.name, "fr"));
  const box = $("#ws-machines");
  box.replaceChildren(...sorted.map((t) => el("label", { class: "check" },
    el("input", { type: "checkbox", name: "ws_target", value: String(t.id), checked: chosen.has(t.id) }),
    el("span", { class: "proto", text: t.protocol }),
    el("span", { text: t.name }),
    t.folder ? el("span", { class: "ws-folder", text: t.folder }) : null)));
  $("#ws-dialog").showModal();
  wsForm.elements.name.focus();
}

wsForm.addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const err = wsForm.querySelector("[data-form-error]");
  const ids = [...wsForm.querySelectorAll("[name=ws_target]:checked")].map((c) => Number(c.value));
  // Ordre : celui de l'espace existant, puis les nouvelles machines dans l'ordre de la liste.
  const before = editingWorkspace ? editingWorkspace.items.map((it) => it.target_id) : [];
  const order = [...before.filter((id) => ids.includes(id)), ...ids.filter((id) => !before.includes(id))];
  const prevSync = new Map((editingWorkspace ? editingWorkspace.items : []).map((it) => [it.target_id, it.sync]));
  const body = {
    name: wsForm.elements.name.value,
    layout: wsForm.elements.layout.value,
    sync: wsForm.elements.sync.checked,
    items: order.map((id) => ({ target_id: id, sync: prevSync.has(id) ? prevSync.get(id) : true })),
  };
  if (!body.items.length) {
    err.textContent = "Choisissez au moins une machine.";
    err.hidden = false;
    return;
  }
  try {
    if (editingWorkspace) await api("PUT", `/api/workspaces/${editingWorkspace.id}`, body);
    else {
      if ((state.workspaces || []).some((x) => x.name.toLowerCase() === body.name.trim().toLowerCase()) &&
          !confirm(`L'espace « ${body.name.trim()} » existe déjà. Le remplacer ?`)) return;
      await api("POST", "/api/workspaces", body);
    }
    $("#ws-dialog").close();
    state.workspaces = await api("GET", "/api/workspaces");
    renderWorkspaces();
  } catch (e) {
    if (e.status === 401) { $("#ws-dialog").close(); return route(); }
    err.textContent = e.message;
    err.hidden = false;
  }
});

async function openWorkspace(w) {
  const byId = new Map(state.targets.map((t) => [t.id, t]));
  const items = w.items.filter((it) => byId.has(it.target_id));
  if (!items.length) return setError("Aucune machine de cet espace ne vous est accessible.");
  const sessions = await sessionsReady();
  showScreen("sessions");
  sessions.openWorkspace(w, items.map((it) => ({ target: byId.get(it.target_id), sync: it.sync })));
  if (items.length < w.items.length) setError("Certaines machines de cet espace n'existent plus ou ne vous sont plus accessibles.");
}

async function loadSessions() {
  const rows = await api("GET", "/api/admin/access-sessions");
  const tbody = $("#sessions");
  tbody.replaceChildren();
  for (const s of rows) {
    tbody.append(el("tr", {},
      el("td", { text: fmtDate(s.started_at) }),
      el("td", { text: s.username }),
      el("td", { text: s.target_name }),
      el("td", { text: fmtDuration(s.started_at, s.ended_at) }),
      el("td", { text: s.ended_at ? s.end_reason : "—", class: s.ended_at ? "" : "ev-ok" }),
      el("td", { text: s.client_ip }),
      el("td", {}, recordingCell(s))));
  }
  state.sessions = rows;
  if (!rows.length) tbody.append(el("tr", {}, el("td", { colSpan: 7, class: "detail", text: "Aucune session." })));
}

async function loadAudit() {
  const rows = await api("GET", "/api/admin/audit?limit=200");
  const tbody = $("#audit");
  tbody.replaceChildren();
  for (const e of rows) {
    const d = e.details || {};
    const detail = [d.target, d.reason, d.error, d.duration_s != null ? `${d.duration_s} s` : null]
      .filter(Boolean).join(" · ");
    const cls = /failed|locked|mismatch/.test(e.event) ? "ev-fail" : /success|session_start/.test(e.event) ? "ev-ok" : "";
    tbody.append(el("tr", {},
      el("td", { text: fmtDate(e.at) }),
      el("td", { text: e.username || "—" }),
      el("td", { text: e.event, class: cls }),
      el("td", { text: detail, class: "detail" }),
      el("td", { text: e.ip })));
  }
}

// ---------- formulaire de cible ----------

const dialog = $("#target-dialog");
const form = $("#target-form");

function setFormError(msg) {
  $("#target-error").textContent = msg || "";
  $("#target-error").hidden = !msg;
}

function setHostKey(line, fp) {
  form.elements.host_key.value = line || "";
  const out = $("#hostkey-fp");
  out.textContent = fp || "Aucune clé enregistrée";
  out.classList.toggle("set", Boolean(fp));
}

const DEFAULT_PORTS = { ssh: "22", vnc: "5900", rdp: "3389" };

function updateAuthFields() {
  const proto = form.elements.protocol.value;
  for (const e of $$("[data-proto]", form)) e.hidden = !e.dataset.proto.split(" ").includes(proto);
  $("#hostkey-title").textContent = proto === "rdp" ? "Certificat RDP de la cible" : "Clé d'hôte SSH";
  // Une méthode d'authentification propre à l'autre protocole revient au mot de passe.
  const method = form.elements.auth_method.value;
  if ((proto !== "ssh" && method === "key") || (proto !== "vnc" && method === "none")) {
    form.elements.auth_method.value = "password";
  }
  const m = form.elements.auth_method.value;
  for (const e of $$("[data-auth]", form)) e.hidden = e.dataset.auth !== m;
  $("#password-label").textContent = proto === "vnc" ? "Mot de passe VNC (8 caractères max)" : "Mot de passe";
  form.elements.password.maxLength = proto === "vnc" ? 8 : 4096;
  $("#secret-hint").hidden = !state.editing || m === "none";
}

function openTargetForm(t) {
  state.editing = t || null;
  form.reset();
  setFormError("");
  $("#target-form-title").textContent = t ? `Modifier « ${t.name} »` : "Nouvelle cible";
  if (t) {
    for (const k of ["name", "host", "port", "username", "description"]) form.elements[k].value = t[k] || "";
    form.elements.protocol.value = t.protocol;
    form.elements.keyboard_layout.value = t.keyboard_layout || "fr";
    form.elements.auth_method.value = t.auth_method;
  }
  fillFolderChoice(t ? t.folder : "");
  setHostKey(t ? t.host_key : "", t ? t.host_key_fingerprint : "");
  const access = (t && t.access) || { users: [], groups: [] };
  renderChecklist($("#target-groups"), state.groups, access.groups, "access_group", "Aucun groupe");
  renderChecklist($("#target-users"), state.users.filter((u) => !u.is_admin), access.users, "access_user",
    "Aucun utilisateur non-admin", (u) => u.username);
  updateAuthFields();
  dialog.showModal();
  form.elements.name.focus();
}

// Dossier : liste déroulante des dossiers existants, ou nouveau dossier.
function fillFolderChoice(current) {
  const sel = $("#folder-choice");
  const folders = state.folders || [];
  sel.replaceChildren(
    el("option", { value: "", text: "Aucun dossier" }),
    ...folders.map((f) => el("option", { value: f, text: f })),
    el("option", { value: "__new__", text: "Nouveau dossier…" }));
  sel.value = current && folders.includes(current) ? current : "";
  form.elements.folder_new.value = "";
  $("#folder-new-label").hidden = true;
}

form.addEventListener("change", (ev) => {
  if (ev.target.name === "folder_choice") {
    const isNew = ev.target.value === "__new__";
    $("#folder-new-label").hidden = !isNew;
    if (isNew) form.elements.folder_new.focus();
  }
  if (ev.target.name === "auth_method") updateAuthFields();
  if (ev.target.name === "protocol") {
    const port = form.elements.port;
    if (Object.values(DEFAULT_PORTS).includes(port.value) || !port.value) port.value = DEFAULT_PORTS[ev.target.value];
    setHostKey("", ""); // empreinte propre au protocole
    updateAuthFields();
  }
  // Changer d'adresse ou de port invalide l'empreinte récupérée.
  if (ev.target.name === "host" || ev.target.name === "port") setHostKey("", "");
});

async function scanHostKey(btn) {
  setFormError("");
  const host = form.elements.host.value.trim();
  if (!host) return setFormError("Renseignez d'abord l'adresse.");
  btn.disabled = true;
  btn.textContent = "Connexion…";
  try {
    const protocol = form.elements.protocol.value;
    const r = await api("POST", "/api/admin/targets/scan", { host, protocol, port: Number(form.elements.port.value) || 0 });
    setHostKey(r.host_key, r.fingerprint);
  } catch (e) {
    setFormError(e.message);
  } finally {
    btn.disabled = false;
    btn.textContent = "Récupérer l'empreinte";
  }
}

form.addEventListener("submit", async (ev) => {
  ev.preventDefault();
  setFormError("");
  const f = form.elements;
  const method = f.auth_method.value;
  const proto = f.protocol.value;
  const body = {
    name: f.name.value, protocol: proto, host: f.host.value, port: Number(f.port.value) || 0,
    username: proto !== "vnc" ? f.username.value : "", auth_method: method,
    host_key: proto !== "vnc" ? f.host_key.value : "", description: f.description.value,
    keyboard_layout: proto === "rdp" ? f.keyboard_layout.value : "",
    folder: f.folder_choice.value === "__new__" ? f.folder_new.value : f.folder_choice.value,
    secret: method === "key" ? f.private_key.value : method === "none" ? "" : f.password.value,
    passphrase: method === "key" ? f.passphrase.value : "",
    access: { users: checkedIds(form, "access_user"), groups: checkedIds(form, "access_group") },
  };
  const btn = form.querySelector("button[type=submit]");
  btn.disabled = true;
  try {
    if (state.editing) await api("PUT", `/api/admin/targets/${state.editing.id}`, body);
    else await api("POST", "/api/admin/targets", body);
    dialog.close();
    await loadTargets();
  } catch (e) {
    if (e.status === 401) { dialog.close(); return route(); }
    setFormError(e.message);
  } finally {
    btn.disabled = false;
  }
});

// ---------- sessions ouvertes (onglets) ----------

// Liens lus par sessions.js (module chargé après ce script, parfois bien plus tard).
window.GnaHooks = {
  tools: () => (state.me && state.me.tools) || {},
  recording: () => (state.me && state.me.recording) || {},
  onHome: () => showHome(),
  onChange: (n) => updateSessionsButton(n),
};

// Le module des sessions (noVNC compris) peut finir de charger après l'affichage.
function sessionsReady() {
  if (window.GnaSessions) return Promise.resolve(window.GnaSessions);
  return new Promise((resolve) => window.addEventListener("gna-sessions-ready", () => resolve(window.GnaSessions), { once: true }));
}

// Retour à la liste des machines ; les sessions restent ouvertes en arrière-plan.
function showHome() {
  showScreen("app");
  updateSessionsButton(window.GnaSessions ? window.GnaSessions.count() : 0);
}

function updateSessionsButton(n) {
  $("#sessions-btn").hidden = n === 0;
  $("#sessions-count").textContent = String(n);
}

async function openSession(target) {
  const sessions = await sessionsReady();
  showScreen("sessions"); // la zone doit être visible pour mesurer sa taille
  sessions.open(target);
}

// ---------- actions ----------

document.addEventListener("click", async (ev) => {
  const tabBtn = ev.target.closest("[data-tab]");
  if (tabBtn) return selectTab(tabBtn.dataset.tab);

  const btn = ev.target.closest("[data-action]");
  if (!btn) return;
  const target = state.targets.find((t) => String(t.id) === btn.dataset.id);

  switch (btn.dataset.action) {
    case "logout":
      if (window.GnaSessions) window.GnaSessions.closeAll();
      try { await api("POST", "/api/auth/logout", {}); } catch { /* session déjà expirée */ }
      return route();
    case "new-target":
      await loadPeople();
      return openTargetForm(null);
    case "edit":
      await loadPeople();
      return openTargetForm(target);
    case "account":
      return openPasswordDialog(null);
    case "new-user":
      $("#user-form").reset();
      formError($("#user-form"), "");
      $("#user-dialog").showModal();
      return;
    case "gen-password":
      $("#user-form").elements.password.value = generatePassword();
      return;
    case "new-group":
      await loadPeople();
      return openGroupForm(null);
    case "edit-group":
      await loadPeople();
      return openGroupForm(state.groups.find((g) => String(g.id) === btn.dataset.id));
    case "delete-group": {
      const g = state.groups.find((x) => String(x.id) === btn.dataset.id);
      if (!g || !confirm(`Supprimer le groupe « ${g.name} » ? Ses membres perdront les accès accordés par ce groupe.`)) return;
      try { await api("DELETE", `/api/admin/groups/${g.id}`); await loadGroups(); } catch (e) { handleAppError(e); }
      return;
    }
    case "close-dialog":
      btn.closest("dialog").close();
      return;
    case "cancel-target":
      return dialog.close();
    case "scan":
      return scanHostKey(btn);
    case "delete":
      if (!target || !confirm(`Supprimer la cible « ${target.name} » et ses identifiants ?`)) return;
      try { await api("DELETE", `/api/admin/targets/${target.id}`); await loadTargets(); } catch (e) { handleAppError(e); }
      return;
    case "open":
      if (!target) return;
      return openSession(target);
    case "favorite": {
      const t = state.targets.find((x) => x.id === Number(btn.dataset.id));
      if (!t) return;
      try {
        await api("PUT", `/api/targets/${t.id}/favorite`, { favorite: !t.favorite });
        t.favorite = !t.favorite;
        renderTargets();
      } catch (e) { handleAppError(e); }
      return;
    }
    case "toggle-group": {
      const g = btn.dataset.group;
      if (org.collapsed.has(g)) org.collapsed.delete(g); else org.collapsed.add(g);
      saveCollapsed();
      return renderTargets();
    }
    case "ws-open": {
      const w = (state.workspaces || []).find((x) => x.id === Number(btn.dataset.id));
      if (w) return openWorkspace(w);
      return;
    }
    case "ws-new":
      return openWorkspaceForm(null);
    case "ws-edit":
      return openWorkspaceForm((state.workspaces || []).find((x) => x.id === Number(btn.dataset.id)));
    case "ws-delete": {
      const w = (state.workspaces || []).find((x) => x.id === Number(btn.dataset.id));
      if (!w || !confirm(`Supprimer l'espace de travail « ${w.name} » ? (les machines ne sont pas touchées)`)) return;
      try {
        await api("DELETE", `/api/workspaces/${w.id}`);
        state.workspaces = state.workspaces.filter((x) => x !== w);
        renderWorkspaces();
      } catch (e) { handleAppError(e); }
      return;
    }
    case "ses-show": {
      const sessions = await sessionsReady();
      showScreen("sessions");
      return sessions.resume();
    }
  }
});

route();

function accessSummary(a) {
  const parts = [];
  if (a.groups.length) parts.push(`${a.groups.length} groupe${a.groups.length > 1 ? "s" : ""}`);
  if (a.users.length) parts.push(`${a.users.length} utilisateur${a.users.length > 1 ? "s" : ""}`);
  return parts.length ? `Accès : ${parts.join(", ")} + admins` : "Accès : administrateurs uniquement";
}

// ---------- utilitaires de formulaires ----------

function formError(form, msg) {
  const e = form.querySelector("[data-form-error]");
  e.textContent = msg || "";
  e.hidden = !msg;
}

function renderChecklist(container, items, checked, name, emptyText, label = (x) => x.name) {
  container.replaceChildren();
  if (!items.length) {
    container.append(el("div", { class: "none", text: emptyText }));
    return;
  }
  const set = new Set((checked || []).map(String));
  for (const it of items) {
    const box = el("input", { type: "checkbox", name, value: String(it.id), checked: set.has(String(it.id)) });
    container.append(el("label", {}, box, label(it)));
  }
}

const checkedIds = (form, name) => $$(`input[name="${name}"]:checked`, form).map((i) => Number(i.value));

// Utilisateurs et groupes sont nécessaires aux listes de cases à cocher.
async function loadPeople() {
  if (!state.me.is_admin) return;
  [state.users, state.groups] = await Promise.all([api("GET", "/api/admin/users"), api("GET", "/api/admin/groups")]);
}

function generatePassword() {
  const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789-_";
  const bytes = crypto.getRandomValues(new Uint8Array(18));
  return [...bytes].map((b) => alphabet[b % alphabet.length]).join("");
}

// ---------- utilisateurs ----------

async function loadUsers() {
  await loadPeople();
  const groupName = Object.fromEntries(state.groups.map((g) => [g.id, g.name]));
  const tbody = $("#users");
  tbody.replaceChildren();
  for (const u of state.users) {
    const self = u.id === state.me.id;
    const etat = u.disabled ? el("span", { class: "pill bad", text: "désactivé" })
      : u.locked ? el("span", { class: "pill warn", text: "verrouillé" })
      : el("span", { class: "pill ok", text: "actif" });
    const sel = el("select", { "data-user": u.id, "aria-label": `Actions pour ${u.username}` },
      el("option", { value: "", text: "Actions…" }));
    const opts = [
      u.is_admin ? ["demote", "Retirer les droits admin"] : ["promote", "Rendre administrateur"],
      u.disabled ? ["enable", "Réactiver"] : ["disable", "Désactiver"],
      u.locked ? ["unlock", "Déverrouiller"] : null,
      ["password", "Définir un mot de passe…"],
      u.mfa ? ["reset-mfa", "Réinitialiser la MFA"] : null,
      ["delete", "Supprimer"],
    ].filter(Boolean).filter(([a]) => !(self && ["demote", "disable", "delete"].includes(a)));
    for (const [v, t] of opts) sel.append(el("option", { value: v, text: t }));
    tbody.append(el("tr", {},
      el("td", {}, el("strong", { text: u.username }), self ? el("span", { class: "muted", text: " (vous)" }) : null),
      el("td", { text: u.is_admin ? "Administrateur" : "Utilisateur" }),
      el("td", {}, el("span", { class: u.mfa ? "pill ok" : "pill warn", text: u.mfa ? "activée" : "à configurer" })),
      el("td", {}, etat),
      el("td", { class: "groups-cell" }, ...(u.groups.length ? u.groups.map((g) => el("span", { class: "pill", text: groupName[g] || "?" }))
        : [el("span", { class: "muted", text: "—" })])),
      el("td", {}, sel)));
  }
}

const USER_CONFIRM = {
  demote: (u) => `Retirer les droits administrateur de « ${u} » ?`,
  disable: (u) => `Désactiver « ${u} » ? Ses sessions en cours seront fermées.`,
  "reset-mfa": (u) => `Réinitialiser la double authentification de « ${u} » ? Il devra la reconfigurer à sa prochaine connexion.`,
  delete: (u) => `Supprimer définitivement « ${u} » ? Le journal d'audit conserve son historique.`,
};

document.addEventListener("change", async (ev) => {
  const sel = ev.target.closest("select[data-user]");
  if (!sel || !sel.value) return;
  const action = sel.value;
  sel.value = "";
  const u = state.users.find((x) => String(x.id) === sel.dataset.user);
  if (!u) return;
  if (action === "password") return openPasswordDialog(u);
  if (USER_CONFIRM[action] && !confirm(USER_CONFIRM[action](u.username))) return;
  try {
    await api("POST", `/api/admin/users/${u.id}/${action}`, {});
    await loadUsers();
  } catch (e) {
    handleAppError(e);
  }
});

$("#user-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const f = ev.target;
  formError(f, "");
  try {
    await api("POST", "/api/admin/users", {
      username: f.elements.username.value, password: f.elements.password.value, is_admin: f.elements.is_admin.checked,
    });
    $("#user-dialog").close();
    await loadUsers();
  } catch (e) {
    formError(f, e.message);
  }
});

// ---------- mot de passe (Mon compte ou réinitialisation par un admin) ----------

let passwordTarget = null;

// ---------- thème ----------

function applyTheme(name) {
  const root = document.documentElement;
  if (name === "system") delete root.dataset.theme;
  else root.dataset.theme = name;
  try { localStorage.setItem("gna.theme", name); } catch { /* stockage indisponible */ }
  const sel = $("#theme-select");
  if (sel) sel.value = name;
  window.dispatchEvent(new Event("gna-theme")); // terminaux et bureaux ouverts
}

document.addEventListener("change", async (ev) => {
  if (ev.target.id !== "theme-select") return;
  const theme = ev.target.value;
  applyTheme(theme);
  try {
    await api("PUT", "/api/me/prefs", { theme });
    if (state.me) state.me.prefs = { ...(state.me.prefs || {}), theme };
  } catch (e) {
    formError($("#password-form"), e.message);
  }
});

function openPasswordDialog(user) {
  passwordTarget = user;
  const f = $("#password-form");
  f.reset();
  formError(f, "");
  const self = !user;
  $("#password-title").textContent = self ? "Mon compte" : `Mot de passe de « ${user.username} »`;
  $("#password-intro").textContent = self
    ? `Connecté en tant que ${state.me.username}. Changer le mot de passe ferme vos sessions sur les autres appareils.`
    : "Ses sessions en cours seront fermées. Transmettez-lui le nouveau mot de passe par un canal sûr.";
  for (const e of $$("[data-self]", f)) e.hidden = !self;
  $("#theme-select").value = (state.me && state.me.prefs && state.me.prefs.theme) || "system";
  $("#password-dialog").showModal();
  (self ? f.elements.current : f.elements.new).focus();
}

$("#password-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const f = ev.target;
  formError(f, "");
  if (f.elements.new.value !== f.elements.confirm.value) return formError(f, "Les deux mots de passe ne correspondent pas.");
  try {
    if (passwordTarget) {
      await api("POST", `/api/admin/users/${passwordTarget.id}/password`, { password: f.elements.new.value });
    } else {
      await api("POST", "/api/me/password", { current: f.elements.current.value, new: f.elements.new.value });
    }
    $("#password-dialog").close();
    if (passwordTarget) await loadUsers();
  } catch (e) {
    if (e.status === 401) { $("#password-dialog").close(); return route(); }
    formError(f, e.message);
  }
});

// ---------- groupes ----------

async function loadGroups() {
  await loadPeople();
  if (!state.targets.length) state.targets = await api("GET", "/api/targets");
  const list = $("#groups");
  list.replaceChildren();
  $("#groups-empty").hidden = state.groups.length > 0;
  const plural = (n, w) => `${n} ${w}${n > 1 ? "s" : ""}`;
  for (const g of state.groups) {
    list.append(el("li", { class: "target" },
      el("div", { class: "target-head" }, el("strong", { text: g.name })),
      g.description ? el("div", { class: "target-desc", text: g.description }) : null,
      el("div", { class: "group-meta", text: `${plural(g.members.length, "membre")} · ${plural(g.targets.length, "cible")}` }),
      el("div", { class: "target-actions" },
        el("button", { class: "ghost", type: "button", text: "Modifier", "data-action": "edit-group", "data-id": g.id }),
        el("button", { class: "danger", type: "button", text: "Supprimer", "data-action": "delete-group", "data-id": g.id }))));
  }
}

async function openGroupForm(g) {
  state.editingGroup = g || null;
  if (!state.targets.length) state.targets = await api("GET", "/api/targets");
  const f = $("#group-form");
  f.reset();
  formError(f, "");
  $("#group-title").textContent = g ? `Modifier « ${g.name} »` : "Nouveau groupe";
  if (g) {
    f.elements.name.value = g.name;
    f.elements.description.value = g.description;
  }
  renderChecklist($("#group-members"), state.users, g ? g.members : [], "member", "Aucun utilisateur",
    (u) => u.username + (u.is_admin ? " (admin)" : ""));
  renderChecklist($("#group-targets"), state.targets, g ? g.targets : [], "group_target", "Aucune cible",
    (t) => `${t.name} (${t.protocol.toUpperCase()})`);
  $("#group-dialog").showModal();
  f.elements.name.focus();
}

$("#group-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const f = ev.target;
  formError(f, "");
  const body = {
    name: f.elements.name.value, description: f.elements.description.value,
    members: checkedIds(f, "member"), targets: checkedIds(f, "group_target"),
  };
  try {
    if (state.editingGroup) await api("PUT", `/api/admin/groups/${state.editingGroup.id}`, body);
    else await api("POST", "/api/admin/groups", body);
    $("#group-dialog").close();
    await loadGroups();
  } catch (e) {
    if (e.status === 401) { $("#group-dialog").close(); return route(); }
    formError(f, e.message);
  }
});

// ---------- enregistrements ----------

const fmtSize = (n) => n < 1024 ? `${n} o` : n < 1048576 ? `${(n / 1024).toFixed(0)} Ko` : `${(n / 1048576).toFixed(1)} Mo`;
const fmtClock = (t) => {
  const s = Math.floor(t);
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), sec = s % 60;
  return (h ? `${h}:${String(m).padStart(2, "0")}` : String(m)) + ":" + String(sec).padStart(2, "0");
};

function recordingCell(s) {
  if (s.has_recording) {
    if (!s.ended_at) return el("span", { class: "rec", text: "● en cours" });
    return el("button", { class: "ghost small", type: "button", text: `Rejouer · ${fmtSize(s.recording_size)}`,
      "data-action": "replay", "data-id": s.id });
  }
  if (s.recording_purged) return el("span", { class: "muted", text: "expiré" });
  return el("span", { class: "muted", text: "—" });
}

const replay = { player: null, session: null };

function openReplay(session, startAt = 0) {
  replay.session = session;
  showScreen("replay");
  $("#replay-name").textContent = `${session.username} → ${session.target_name}`;
  $("#replay-info").textContent = `${session.protocol.toUpperCase()} · ${fmtDate(session.started_at)} · ${fmtDuration(session.started_at, session.ended_at)}`;
  const vnc = session.protocol === "vnc" || session.protocol === "rdp"; // même lecteur
  $("#player").hidden = vnc;
  $("#replay-side").hidden = vnc;
  $("#vnc-player").hidden = !vnc;
  $("[data-action=replay-download]").hidden = vnc;
  if (vnc) return window.GnaVncReplay.open(session);
  $("#replay-hits").replaceChildren();
  $("#replay-search").reset();
  const box = $("#player");
  box.replaceChildren();
  if (replay.player) replay.player.dispose();
  replay.player = AsciinemaPlayer.create(`/api/admin/recordings/${session.id}/cast`, box, {
    autoPlay: true, startAt, fit: "both", terminalFontFamily: getComputedStyle(document.documentElement).getPropertyValue("--mono"),
    theme: "asciinema", controls: true,
    // Pas d'idleTimeLimit : les horodatages des résultats de recherche doivent rester en temps réel.
  });
}

function closeReplay() {
  if (replay.player) { replay.player.dispose(); replay.player = null; }
  window.GnaVncReplay.close();
  replay.session = null;
  showScreen("app");
}

function seekTo(t) {
  if (!replay.player) return;
  replay.player.seek(Math.max(0, t - 0.5)).then(() => replay.player.play());
}

function hitButton(h, onClick) {
  return el("li", {}, (() => {
    const b = el("button", { class: "hit", type: "button" }, el("time", { text: fmtClock(h.time) }), el("code", { text: h.line }));
    b.addEventListener("click", onClick);
    return b;
  })());
}

$("#replay-search").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const q = ev.target.elements.q.value;
  const list = $("#replay-hits");
  list.replaceChildren();
  try {
    const hits = await api("GET", `/api/admin/recordings/${replay.session.id}/search?q=${encodeURIComponent(q)}`);
    if (!hits.length) list.append(el("li", { class: "hint", text: "Aucun résultat." }));
    for (const h of hits) list.append(hitButton(h, () => seekTo(h.time)));
  } catch (e) {
    list.append(el("li", { class: "error", text: e.message }));
  }
});

$("#rec-search").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const f = ev.target;
  const box = $("#rec-results");
  box.hidden = false;
  box.replaceChildren(el("p", { class: "hint", text: "Recherche en cours…" }));
  try {
    const res = await api("GET", `/api/admin/recordings/search?q=${encodeURIComponent(f.elements.q.value)}&days=${f.elements.days.value}`);
    box.replaceChildren();
    if (!res.length) box.append(el("p", { class: "hint", text: "Aucune session ne contient ce texte." }));
    for (const r of res) {
      const hits = el("ol", { class: "hits" });
      for (const h of r.hits) hits.append(hitButton(h, () => openReplay(r, Math.max(0, h.time - 0.5))));
      box.append(el("div", { class: "rec-result" },
        el("div", { class: "rec-result-head" },
          el("strong", { text: `${r.username} → ${r.target_name}` }),
          el("span", { class: "muted", text: `${fmtDate(r.started_at)} · ${r.total} résultat${r.total > 1 ? "s" : ""}` })),
        hits));
    }
  } catch (e) {
    box.replaceChildren(el("p", { class: "error", text: e.message }));
  }
});

document.addEventListener("click", (ev) => {
  const btn = ev.target.closest("[data-action]");
  if (!btn) return;
  if (btn.dataset.action === "replay") {
    const s = (state.sessions || []).find((x) => String(x.id) === btn.dataset.id);
    if (s) openReplay(s);
  } else if (btn.dataset.action === "replay-close") {
    closeReplay();
  } else if (btn.dataset.action === "replay-download" && replay.session) {
    const a = el("a", { href: `/api/admin/recordings/${replay.session.id}/cast`, download: `gna-session-${replay.session.id}.cast` });
    document.body.append(a);
    a.click();
    a.remove();
  }
});

document.addEventListener("input", (ev) => {
  if (ev.target.id !== "target-search") return;
  org.search = ev.target.value;
  renderTargets();
});

// Espace enregistré depuis l'écran des sessions : la liste est mise à jour.
window.addEventListener("gna-workspaces-changed", () => {
  if (state.me) api("GET", "/api/workspaces").then((w) => { state.workspaces = w; renderWorkspaces(); }, () => {});
});
