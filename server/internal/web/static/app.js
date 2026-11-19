"use strict";

// Gná — interface web. Même ergonomie que Saga et Andvari : barre du haut avec pages
// (#/machines, #/sessions, #/journal, #/parametres), paramètres à menu latéral,
// dialogues à en-tête, notifications éphémères, bascule clair/sombre.

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];
const state = { me: null, targets: [], users: [], groups: [], page: "machines", editing: null, editingGroup: null };

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

// Icônes (mêmes tracés que Saga) : chaînes fixes, jamais de donnée utilisateur.
const ICONS = {
  edit: '<path d="M12 20h9M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4 12.5-12.5Z"/>',
  trash: '<path d="M3 6h18M8 6V4h8v2M19 6l-1 14H6L5 6"/>',
  play: '<path d="M6 4l14 8-14 8V4Z"/>',
  terminal: '<path d="m4 17 6-6-6-6M12 19h8"/>',
  desktop: '<rect x="2" y="4" width="20" height="13" rx="2"/><path d="M8 21h8M12 17v4"/>',
  sun: '<circle cx="12" cy="12" r="4.5"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/>',
  moon: '<path d="M20 14.5A8.5 8.5 0 0 1 9.5 4a8.5 8.5 0 1 0 10.5 10.5Z"/>',
  chevron: '<path d="m9 18 6-6-6-6"/>',
  shield: '<path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10Z"/><path d="m9 12 2 2 4-4"/>',
};
function icon(name, size = 16) {
  const t = document.createElement("template");
  t.innerHTML = `<svg viewBox="0 0 24 24" width="${size}" height="${size}" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${ICONS[name] || ""}</svg>`;
  return t.content.firstChild;
}

function setError(msg) {
  for (const e of $$("[data-error]")) {
    e.textContent = msg || "";
    e.hidden = !msg;
  }
}

// Notifications éphémères (comme Saga).
// Le conteneur est un « popover » : il passe au-dessus des dialogues ouverts.
function toast(msg, kind = "") {
  const box = $("#toasts");
  const t = el("div", { class: `toast ${kind}`, text: msg });
  box.append(t);
  if (box.showPopover) {
    if (box.matches(":popover-open")) box.hidePopover();
    box.showPopover();
  }
  setTimeout(() => {
    t.remove();
    if (!box.children.length && box.hidePopover && box.matches(":popover-open")) box.hidePopover();
  }, 4200);
}

const fmtDate = (s) => new Date(s).toLocaleString("fr-FR", { day: "2-digit", month: "2-digit", year: "numeric", hour: "2-digit", minute: "2-digit", second: "2-digit" });
function fmtDuration(a, b) {
  if (!b) return "en cours";
  const s = Math.round((new Date(b) - new Date(a)) / 1000);
  if (s < 60) return `${s} s`;
  if (s < 3600) return `${Math.floor(s / 60)} min ${s % 60} s`;
  return `${Math.floor(s / 3600)} h ${Math.floor((s % 3600) / 60)} min`;
}
const plural = (n, w) => `${n} ${w}${n > 1 ? "s" : ""}`;

// ---------- écrans ----------

function showScreen(name) {
  $("#auth").hidden = name !== "auth";
  $("#app").hidden = name !== "app";
  $("#session-view").hidden = name !== "sessions";
  $("#replay-view").hidden = name !== "replay";
}

function showAuthView(view) {
  showScreen("auth");
  for (const v of ["login", "mfa", "enroll"]) $(`#view-${v}`).hidden = v !== view;
  let first = $(`#view-${view} input`);
  // Comme Saga : l'identifiant de la dernière connexion est prérempli.
  if (view === "login") {
    let last = "";
    try { last = localStorage.getItem("gna.user") || ""; } catch { /* stockage indisponible */ }
    const f = $("#view-login");
    if (last && !f.elements.username.value) f.elements.username.value = last;
    try { f.elements.remember.checked = localStorage.getItem("gna.remember") === "1"; } catch { /* idem */ }
    if (f.elements.username.value) first = f.elements.password;
  }
  if (first) first.focus();
}

// État de la session : écran de connexion, MFA, ou application.
async function boot() {
  setError("");
  let me;
  try {
    me = await api("GET", "/api/me");
  } catch {
    state.me = null;
    return showAuthView("login");
  }
  if (me.stage === "mfa") return showAuthView("mfa");
  if (me.stage === "enroll") return startEnroll();
  state.me = me;
  applyTheme((me.prefs && me.prefs.theme) || "auto");
  $("#me-name").textContent = me.username;
  $("#me-name").title = `${me.is_admin ? "Administrateur" : "Utilisateur"} · Paramètres du compte`;
  for (const e of $$("[data-admin]")) e.hidden = !me.is_admin;
  showScreen("app");
  go();
}

// ---------- session expirée ----------

// Vérifie la session toutes les 30 s (et au retour sur la fenêtre) sans la prolonger :
// dès qu'elle a expiré, retour à l'écran de connexion. L'adresse (#/…) est gardée, la
// page revient après la reconnexion.
async function checkSession() {
  if (!state.me) return;
  try {
    const res = await fetch("/api/auth/session", { credentials: "same-origin" });
    if (res.status === 401) sessionExpired();
  } catch { /* réseau indisponible : nouvel essai au prochain tour */ }
}
setInterval(checkSession, 30000);
document.addEventListener("visibilitychange", () => { if (!document.hidden) checkSession(); });

function sessionExpired() {
  if (!state.me) return;
  state.me = null;
  if (window.GnaSessions) window.GnaSessions.closeAll();
  if (!$("#replay-view").hidden) closeReplay();
  for (const d of $$("dialog[open]")) d.close();
  showAuthView("login");
  setError("Votre session a expiré. Reconnectez-vous pour continuer.");
}

// ---------- pages (#/…) ----------

const PAGES = {
  machines: { load: loadTargets },
  sessions: { load: loadSessions, admin: true },
  journal: { load: loadAudit, admin: true },
  parametres: { load: loadSettings },
};

function go() {
  if (!state.me) return;
  for (const d of $$("dialog[open]")) d.close();
  // #/machines/<id> ouvre directement une session (liens gna://machine/<id> de l'application).
  const [first, arg] = location.hash.replace(/^#\/?/, "").split("/");
  let page = first || "machines";
  if (!PAGES[page] || (PAGES[page].admin && !state.me.is_admin)) page = "machines";
  const changed = page !== state.page;
  state.page = page;
  for (const a of $$("#nav a")) a.classList.toggle("on", a.dataset.r === page);
  for (const p of $$("[data-page]")) p.hidden = p.dataset.page !== page;
  setError("");
  if (changed) window.scrollTo(0, 0);
  const loaded = PAGES[page].load();
  if (page === "machines" && arg) loaded.then(() => openFromLink(arg), () => {});
  loaded.catch(handleAppError);
}

function openFromLink(arg) {
  history.replaceState(null, "", "#/machines"); // un rechargement ne rouvre pas la session
  const t = /^\d+$/.test(arg) && state.targets.find((x) => x.id === Number(arg));
  if (!t) return toast("Machine introuvable ou non attribuée à votre compte.", "err");
  openSession(t);
}

window.addEventListener("hashchange", () => {
  if (!state.me) return;
  showHome();
  go();
});

function handleAppError(e) {
  if (e.status === 401) return state.me ? sessionExpired() : boot();
  toast(e.message, "err");
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
    const label = btn.textContent;
    btn.disabled = true;
    btn.replaceChildren(el("span", { class: "spinner" }));
    setError("");
    try {
      await handler(new FormData(form));
      form.reset();
      await boot();
    } catch (e) {
      setError(e.message);
    } finally {
      btn.disabled = false;
      btn.textContent = label;
    }
  });
}

onSubmit($("#view-login"), async (f) => {
  const remember = f.get("remember") === "on";
  await api("POST", "/api/auth/login", { username: f.get("username"), password: f.get("password"), remember });
  try {
    localStorage.setItem("gna.user", f.get("username").trim());
    localStorage.setItem("gna.remember", remember ? "1" : "0");
  } catch { /* facultatif */ }
});
onSubmit($("#view-mfa"), (f) => api("POST", "/api/auth/totp/verify", { code: f.get("code") }));
onSubmit($("#view-enroll"), (f) => api("POST", "/api/auth/totp/enroll/confirm", { code: f.get("code") }));

// ---------- machines : favoris, dossiers, recherche ----------

const org = { search: "", collapsed: new Set() };
try { org.collapsed = new Set(JSON.parse(localStorage.getItem("gna.collapsed") || "[]")); } catch { /* stockage indisponible */ }
const saveCollapsed = () => { try { localStorage.setItem("gna.collapsed", JSON.stringify([...org.collapsed])); } catch { /* idem */ } };

async function loadTargets() {
  const [targets, workspaces] = await Promise.all([api("GET", "/api/targets"), api("GET", "/api/workspaces")]);
  state.targets = targets;
  state.workspaces = workspaces;
  state.folders = [...new Set(targets.map((t) => t.folder).filter(Boolean))].sort((x, y) => x.localeCompare(y, "fr"));
  $("#targets-empty").hidden = targets.length > 0;
  $("#targets-empty-text").textContent = state.me.is_admin
    ? "Ajoutez une première machine : un serveur Linux en SSH, un bureau Windows en RDP ou un bureau VNC."
    : "Aucune machine ne vous est attribuée pour l'instant. Contactez un administrateur.";
  $("#targets-tools").hidden = targets.length < 2;
  $("#ws-new-btn").hidden = !targets.length;
  const sub = [plural(targets.length, "machine")];
  if (state.folders.length) sub.push(plural(state.folders.length, "dossier"));
  if (workspaces.length) sub.push(plural(workspaces.length, "espace") + " de travail");
  $("#targets-sub").textContent = targets.length ? sub.join(" · ") : "";
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
  const open = el("button", { class: "open", type: "button", "data-action": "open", "data-id": t.id },
    icon(isDesktop ? "desktop" : "terminal"), isDesktop ? "Ouvrir le bureau" : "Ouvrir le terminal");
  const actions = el("div", { class: "target-actions" }, open);
  if (state.me.is_admin) {
    actions.append(el("button", { class: "icon-btn", type: "button", "data-action": "edit", "data-id": t.id,
      title: "Modifier", "aria-label": `Modifier ${t.name}` }, icon("edit")));
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
      // Machine ajoutée par une liaison (Völund) : identifiants encore à saisir.
      t.auth_method !== "none" && !t.has_secret
        ? el("span", { class: "badge warn", text: "à compléter", title: "Aucun identifiant de connexion : un administrateur doit les saisir (Modifier)" }) : null,
      star),
    el("div", { class: "target-addr", text: isVNC ? `${t.host}:${t.port}` : `${t.username}@${t.host}:${t.port}` }),
    t.description ? el("div", { class: "target-desc", text: t.description }) : null,
    isVNC ? null : el("div", { class: "target-fp", text: t.host_key_fingerprint || "empreinte non enregistrée" }),
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
  if (loose.length) groups.push({ key: "-", title: folders.length || favs.length ? "Sans dossier" : "", items: loose });
  const searching = Boolean(org.search.trim());
  for (const g of groups) {
    const collapsed = !searching && org.collapsed.has(g.key);
    const ul = el("ul", { class: "targets" }, ...g.items.map(targetCard));
    ul.hidden = collapsed;
    const section = el("section", { class: "tgroup" });
    if (g.title) {
      const head = el("button", {
        type: "button", class: "tgroup-head" + (collapsed ? "" : " open"), "data-action": "toggle-group", "data-group": g.key,
        "aria-expanded": String(!collapsed),
      }, icon("chevron", 14), el("span", { text: g.title }), el("span", { class: "n", text: String(g.items.length) }));
      section.append(head);
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
    const meta = [plural(w.items.length, "session"), w.layout === "grid" ? "mosaïque" : "onglets"];
    if (w.sync) meta.push("saisie synchronisée");
    return el("li", { class: "ws" },
      el("div", { class: "ws-main" },
        el("strong", { text: w.name }),
        el("span", { class: "ws-meta", text: meta.join(" · ") }),
        el("span", { class: "ws-targets", text: names.join(", ") || "machines indisponibles" })),
      el("div", { class: "ws-actions" },
        el("button", { type: "button", class: "small", "data-action": "ws-open", "data-id": w.id }, icon("play", 14), "Ouvrir"),
        el("button", { type: "button", class: "icon-btn", "data-action": "ws-edit", "data-id": w.id, title: "Modifier", "aria-label": `Modifier ${w.name}` }, icon("edit")),
        el("button", { type: "button", class: "icon-btn danger", "data-action": "ws-delete", "data-id": w.id, title: "Supprimer", "aria-label": `Supprimer ${w.name}` }, icon("trash"))));
  }));
}

// Création / modification d'un espace sans ouvrir de connexion.
let editingWorkspace = null;
const wsForm = $("#ws-form");

function openWorkspaceForm(w) {
  editingWorkspace = w || null;
  wsForm.reset();
  formError(wsForm, "");
  $("#ws-form-title").textContent = w ? `Modifier « ${w.name} »` : "Nouvel espace de travail";
  wsForm.elements.name.value = w ? w.name : "";
  wsForm.elements.layout.value = w ? w.layout : "tabs";
  wsForm.elements.sync.checked = w ? w.sync : false;
  const chosen = new Set(w ? w.items.map((it) => it.target_id) : []);
  // Machines rangées par dossier, comme dans la liste.
  const sorted = [...state.targets].sort((a, b) =>
    (a.folder || "￿").localeCompare(b.folder || "￿", "fr") || a.name.localeCompare(b.name, "fr"));
  $("#ws-machines").replaceChildren(...sorted.map((t) => el("label", { class: "check" },
    el("input", { type: "checkbox", name: "ws_target", value: String(t.id), checked: chosen.has(t.id) }),
    el("span", { class: "proto", text: t.protocol }),
    el("span", { text: t.name }),
    t.folder ? el("span", { class: "ws-folder", text: t.folder }) : null)));
  $("#ws-dialog").showModal();
  wsForm.elements.name.focus();
}

wsForm.addEventListener("submit", async (ev) => {
  ev.preventDefault();
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
  if (!body.items.length) return formError(wsForm, "Choisissez au moins une machine.");
  try {
    if (editingWorkspace) await api("PUT", `/api/workspaces/${editingWorkspace.id}`, body);
    else {
      if ((state.workspaces || []).some((x) => x.name.toLowerCase() === body.name.trim().toLowerCase()) &&
          !confirm(`L'espace « ${body.name.trim()} » existe déjà. Le remplacer ?`)) return;
      await api("POST", "/api/workspaces", body);
    }
    $("#ws-dialog").close();
    toast("Espace de travail enregistré", "ok");
    await loadTargets();
  } catch (e) {
    if (e.status === 401) { $("#ws-dialog").close(); return boot(); }
    formError(wsForm, e.message);
  }
});

async function openWorkspace(w) {
  const byId = new Map(state.targets.map((t) => [t.id, t]));
  const items = w.items.filter((it) => byId.has(it.target_id));
  if (!items.length) return toast("Aucune machine de cet espace ne vous est accessible.", "err");
  const sessions = await sessionsReady();
  showScreen("sessions");
  sessions.openWorkspace(w, items.map((it) => ({ target: byId.get(it.target_id), sync: it.sync })));
  if (items.length < w.items.length) toast("Certaines machines de cet espace n'existent plus ou ne vous sont plus accessibles.", "err");
}

// ---------- sessions d'accès et journal (admin) ----------

async function loadSessions() {
  const rows = await api("GET", "/api/admin/access-sessions");
  const tbody = $("#sessions");
  tbody.replaceChildren();
  for (const s of rows) {
    tbody.append(el("tr", {},
      el("td", { class: "nowrap", text: fmtDate(s.started_at) }),
      el("td", {}, el("strong", { text: s.username })),
      el("td", { text: s.target_name }),
      el("td", { class: "nowrap", text: fmtDuration(s.started_at, s.ended_at) }),
      el("td", {}, s.ended_at ? el("span", { class: "muted", text: s.end_reason }) : el("span", { class: "badge ok", text: "en cours" })),
      el("td", { class: "hide-sm mono small", text: s.client_ip }),
      el("td", {}, recordingCell(s))));
  }
  state.sessions = rows;
  if (!rows.length) tbody.append(el("tr", {}, el("td", { colSpan: 7, class: "empty-cell", text: "Aucune session pour l'instant." })));
}

async function loadAudit() {
  const rows = await api("GET", "/api/admin/audit?limit=200");
  const tbody = $("#audit");
  tbody.replaceChildren();
  for (const e of rows) {
    const d = e.details || {};
    const detail = [d.target, d.reason, d.error, d.duration_s != null ? `${d.duration_s} s` : null]
      .filter(Boolean).join(" · ");
    const cls = /failed|locked|mismatch|denied/.test(e.event) ? "badge down" : /success|session_start/.test(e.event) ? "badge ok" : "badge";
    tbody.append(el("tr", {},
      el("td", { class: "nowrap", text: fmtDate(e.at) }),
      el("td", { text: e.username || "—" }),
      el("td", {}, el("span", { class: cls, text: e.event })),
      el("td", { text: detail, class: "detail" }),
      el("td", { class: "hide-sm mono small", text: e.ip })));
  }
  if (!rows.length) tbody.append(el("tr", {}, el("td", { colSpan: 5, class: "empty-cell", text: "Journal vide." })));
}

// ---------- formulaire de machine ----------

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
  $("#hostkey-title").textContent = proto === "rdp" ? "Certificat RDP de la machine" : "Clé d'hôte SSH";
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
  $("#target-form-title").textContent = t ? `Modifier « ${t.name} »` : "Nouvelle machine";
  $("#target-delete").hidden = !t;
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
    toast(state.editing ? "Machine enregistrée" : "Machine ajoutée", "ok");
    await loadTargets();
  } catch (e) {
    if (e.status === 401) { dialog.close(); return boot(); }
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

// Retour aux pages ; les sessions restent ouvertes en arrière-plan.
function showHome() {
  if (!$("#replay-view").hidden) closeReplay();
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
  // Fermeture des dialogues (✕, Annuler, Fermer).
  const close = ev.target.closest("[data-close], [data-action=close-dialog]");
  if (close) return close.closest("dialog").close();

  const btn = ev.target.closest("[data-action]");
  if (!btn) return;
  const target = state.targets.find((t) => String(t.id) === btn.dataset.id);

  switch (btn.dataset.action) {
    case "logout":
      if (window.GnaSessions) window.GnaSessions.closeAll();
      try { await api("POST", "/api/auth/logout", {}); } catch { /* session déjà expirée */ }
      state.me = null;
      return boot();
    case "theme-toggle":
      return setTheme(LIGHT.has(effectiveTheme()) ? "gna" : "ecume");
    case "reload":
      return PAGES[state.page].load().then(() => toast("Actualisé", "ok"), handleAppError);
    case "new-target":
      await loadPeople();
      return openTargetForm(null);
    case "edit":
      await loadPeople();
      return openTargetForm(target);
    case "delete-from-form": {
      const t = state.editing;
      if (!t || !confirm(`Supprimer la machine « ${t.name} » et ses identifiants ?`)) return;
      try {
        await api("DELETE", `/api/admin/targets/${t.id}`);
        dialog.close();
        toast("Machine supprimée", "ok");
        await loadTargets();
      } catch (e) { setFormError(e.message); }
      return;
    }
    case "new-user":
      $("#user-form").reset();
      formError($("#user-form"), "");
      $("#user-dialog").showModal();
      $("#user-form").elements.username.focus();
      return;
    case "gen-password":
      $("#user-form").elements.password.value = generatePassword();
      return;
    case "manage-user":
      return openUserManage(state.users.find((u) => String(u.id) === btn.dataset.id));
    case "user-act":
      return userAction(btn.dataset.act);
    case "new-group":
      await loadPeople();
      return openGroupForm(null);
    case "edit-group":
      await loadPeople();
      return openGroupForm(state.groups.find((g) => String(g.id) === btn.dataset.id));
    case "delete-group": {
      const g = state.editingGroup;
      if (!g || !confirm(`Supprimer le groupe « ${g.name} » ? Ses membres perdront les accès accordés par ce groupe.`)) return;
      try {
        await api("DELETE", `/api/admin/groups/${g.id}`);
        $("#group-dialog").close();
        toast("Groupe supprimé", "ok");
        await loadSettings();
      } catch (e) { formError($("#group-form"), e.message); }
      return;
    }
    case "cmds-library": {
      const sessions = await sessionsReady();
      await sessions.openSnippets();
      $("#snippets-dialog").addEventListener("close", () => { if (state.page === "parametres") loadCmdsCount(); }, { once: true });
      return;
    }
    case "scan":
      return scanHostKey(btn);
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
        toast("Espace de travail supprimé", "ok");
        await loadTargets();
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

// « / » place le curseur dans la recherche (comme Andvari) ; Échap la vide.
document.addEventListener("keydown", (ev) => {
  const s = $("#target-search");
  const typing = ev.target instanceof HTMLInputElement || ev.target instanceof HTMLTextAreaElement || ev.target.isContentEditable;
  if (ev.key === "/" && !typing && !$("#app").hidden && state.page === "machines" && !$("#targets-tools").hidden && !$("dialog[open]")) {
    ev.preventDefault();
    s.focus();
  } else if (ev.key === "Escape" && ev.target === s) {
    s.value = "";
    org.search = "";
    renderTargets();
    s.blur();
  }
});

boot();

function accessSummary(a) {
  const parts = [];
  if (a.groups.length) parts.push(plural(a.groups.length, "groupe"));
  if (a.users.length) parts.push(plural(a.users.length, "utilisateur"));
  return parts.length ? `Accès : ${parts.join(", ")} + admins` : "Accès : administrateurs uniquement";
}

// ---------- utilitaires de formulaires ----------

function formError(f, msg) {
  const e = f.querySelector("[data-form-error]");
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

const checkedIds = (f, name) => $$(`input[name="${name}"]:checked`, f).map((i) => Number(i.value));

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

// ---------- paramètres ----------

async function loadSettings() {
  const me = state.me;
  $("#account-lead").replaceChildren("Connecté en tant que ", el("strong", { text: me.username }),
    ` · ${me.is_admin ? "Administrateur" : "Utilisateur"}. `,
    me.remember
      ? `Session gardée sur cet appareil jusqu'au ${fmtDate(me.expires_at)}.`
      : `Déconnexion après ${Math.round((me.idle_timeout_s || 1800) / 60)} min d'inactivité (cochez « Rester connecté sur cet appareil » à la connexion pour l'éviter).`);
  $("#self-password-form").elements.username.value = me.username;
  renderThemePick();
  const jobs = [loadCmdsCount(), loadTokens()];
  if (me.is_admin) {
    await loadPeople();
    if (!state.targets.length) state.targets = await api("GET", "/api/targets");
    renderUsers();
    renderGroups();
    jobs.push(loadLinks());
  }
  await Promise.all(jobs);
}

async function loadCmdsCount() {
  try {
    const list = await api("GET", "/api/snippets");
    $("#cmds-count").textContent = list.length ? `${plural(list.length, "commande")} enregistrée${list.length > 1 ? "s" : ""}.` : "Aucune commande enregistrée pour l'instant.";
  } catch { $("#cmds-count").textContent = ""; }
}

// Menu latéral : défilement doux vers la section (comme Saga).
for (const a of $$("#side a")) {
  a.addEventListener("click", (ev) => {
    ev.preventDefault();
    $(a.getAttribute("href")).scrollIntoView({ behavior: "smooth", block: "start" });
    for (const x of $$("#side a")) x.classList.toggle("on", x === a);
  });
}

$("#self-password-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const f = ev.target;
  formError(f, "");
  if (f.elements.new.value !== f.elements.confirm.value) return formError(f, "Les deux mots de passe ne correspondent pas.");
  try {
    await api("POST", "/api/me/password", { current: f.elements.current.value, new: f.elements.new.value });
    f.reset();
    f.elements.username.value = state.me.username;
    toast("Mot de passe changé", "ok");
  } catch (e) {
    if (e.status === 401) return boot();
    formError(f, e.message);
  }
});

// ---------- jetons d'accès personnels ----------

const fmtDay = (s) => new Date(s).toLocaleString("fr-FR", { day: "2-digit", month: "2-digit", year: "numeric", hour: "2-digit", minute: "2-digit" });

async function loadTokens() {
  const list = await api("GET", "/api/me/tokens");
  const box = $("#tokens");
  if (!list.length) {
    box.replaceChildren(el("div", { class: "panel panel-pad muted small", text: "Aucun jeton pour l'instant." }));
    return;
  }
  box.replaceChildren(...list.map((t) => el("div", { class: "list-row" },
    el("span", { class: t.last_used_at ? "dot up" : "dot" }),
    el("div", { class: "grow" },
      el("strong", { text: t.name }),
      el("div", { class: "muted small", text: `Créé le ${fmtDay(t.created_at)} · ` +
        (t.last_used_at ? `dernière utilisation le ${fmtDay(t.last_used_at)}${t.last_used_ip ? ` (${t.last_used_ip})` : ""}` : "jamais utilisé") })),
    el("button", { type: "button", class: "icon-btn danger", "data-action": "token-revoke", "data-id": t.id, "data-name": t.name,
      title: "Révoquer", "aria-label": `Révoquer ${t.name}` }, icon("trash")))));
}

$("#token-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const f = ev.target;
  formError(f, "");
  try {
    const t = await api("POST", "/api/me/tokens", { name: f.elements.name.value });
    f.reset();
    // Affiché une seule fois : le serveur n'en garde que l'empreinte.
    $("#token-value").textContent = t.token;
    $("#token-curl").textContent = `curl -fsS -H "Authorization: Bearer ${t.token}" ${location.origin}/api/targets`;
    $("#token-dialog").showModal();
    await loadTokens();
  } catch (e) {
    if (e.status === 401) return boot();
    formError(f, e.message);
  }
});

$("#token-dialog").addEventListener("close", () => {
  $("#token-value").textContent = "";
  $("#token-curl").textContent = "";
});

async function copyText(text, btn) {
  try {
    await navigator.clipboard.writeText(text);
  } catch {
    const ta = el("textarea", { value: text });
    btn.closest("dialog").append(ta);
    ta.select();
    document.execCommand("copy");
    ta.remove();
  }
  const old = btn.textContent;
  btn.textContent = "Copié ✓";
  setTimeout(() => { btn.textContent = old; }, 1500);
}

document.addEventListener("click", async (ev) => {
  const b = ev.target.closest("[data-action=token-copy], [data-action=token-revoke]");
  if (!b) return;
  if (b.dataset.action === "token-copy") return copyText($(b.dataset.from).textContent, b);
  if (!confirm(`Révoquer le jeton « ${b.dataset.name} » ? Les scripts qui l'utilisent ne pourront plus lire la liste des machines.`)) return;
  try {
    await api("DELETE", `/api/me/tokens/${b.dataset.id}`);
    toast("Jeton révoqué", "ok");
    await loadTokens();
  } catch (e) { handleAppError(e); }
});

// ---------- liaisons Runa (administrateurs) ----------

const LINK_SCOPES = { "targets:write": "Ajouter des machines", "secrets:write": "Recevoir des mots de passe" };

async function loadLinks() {
  const list = await api("GET", "/api/admin/links");
  const box = $("#links");
  if (!list.length) {
    box.replaceChildren(el("div", { class: "panel panel-pad muted small", text: "Aucune liaison : aucun autre outil n'écrit dans Gná." }));
    return;
  }
  box.replaceChildren(...list.map((l) => el("div", { class: "list-row" },
    el("span", { class: l.pending_until ? "dot" : "dot up" }),
    el("div", { class: "grow" },
      el("strong", { text: l.name }), " ",
      ...l.scopes.map((s) => el("span", { class: "badge", text: LINK_SCOPES[s] || s })),
      l.pending_until ? el("span", { class: "badge warn", text: "en attente d'appairage" }) : "",
      el("div", { class: "muted small", text: (l.pending_until
        ? `Code valable jusqu'à ${new Date(l.pending_until).toLocaleTimeString("fr-FR", { hour: "2-digit", minute: "2-digit" })}`
        : (l.last_used_at ? `Dernière utilisation le ${fmtDay(l.last_used_at)}${l.last_used_ip ? ` (${l.last_used_ip})` : ""}` : "jamais utilisée"))
        + ` · créée par ${l.created_by}` })),
    el("button", { type: "button", class: "icon-btn danger", "data-action": "link-revoke", "data-id": l.id, "data-name": l.name,
      title: "Révoquer", "aria-label": `Révoquer ${l.name}` }, icon("trash")))));
}

document.addEventListener("click", async (ev) => {
  const b = ev.target.closest("[data-action=new-link], [data-action=link-revoke]");
  if (!b) return;
  if (b.dataset.action === "new-link") {
    const d = $("#link-dialog");
    const f = $("#link-form");
    f.reset();
    formError(f, "");
    f.hidden = false;
    $("#link-done").hidden = true;
    d.showModal();
    f.elements.name.focus();
    return;
  }
  if (!confirm(`Révoquer la liaison « ${b.dataset.name} » ? L'outil lié ne pourra plus ajouter de machines. Les machines déjà ajoutées restent.`)) return;
  try {
    await api("DELETE", `/api/admin/links/${b.dataset.id}`);
    toast("Liaison révoquée", "ok");
    await loadLinks();
  } catch (e) { handleAppError(e); }
});

$("#link-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const f = ev.target;
  formError(f, "");
  try {
    const scopes = [...f.querySelectorAll("[name=scope]:checked")].map((c) => c.value);
    const r = await api("POST", "/api/admin/links", { name: f.elements.name.value, scopes });
    $("#link-code").textContent = r.code;
    f.hidden = true;
    $("#link-done").hidden = false;
    await loadLinks();
  } catch (e) {
    if (e.status === 401) return boot();
    formError(f, e.message);
  }
});

$("#link-dialog").addEventListener("close", () => { $("#link-code").textContent = ""; });

// ---------- utilisateurs ----------

function renderUsers() {
  const groupName = Object.fromEntries(state.groups.map((g) => [g.id, g.name]));
  $("#users").replaceChildren(...state.users.map((u) => {
    const self = u.id === state.me.id;
    const dot = u.disabled ? "down" : u.locked ? "warn" : "up";
    const badges = [
      el("span", { class: u.is_admin ? "badge accent" : "badge", text: u.is_admin ? "Administrateur" : "Utilisateur" }),
      el("span", { class: u.mfa ? "badge ok" : "badge warn", text: u.mfa ? "MFA" : "MFA à configurer" }),
      u.disabled ? el("span", { class: "badge down", text: "désactivé" }) : null,
      u.locked ? el("span", { class: "badge warn", text: "verrouillé" }) : null,
      self ? el("span", { class: "muted small", text: "(vous)" }) : null,
    ];
    const groups = u.groups.map((g) => groupName[g]).filter(Boolean);
    return el("div", { class: "list-row" },
      el("span", { class: `dot ${dot}` }),
      el("div", { class: "grow" },
        el("div", { class: "line" }, el("strong", { text: u.username }), ...badges),
        el("div", { class: "muted small", text: groups.length ? `Groupes : ${groups.join(", ")}` : "Aucun groupe" })),
      el("button", { type: "button", class: "icon-btn plain", "data-action": "manage-user", "data-id": u.id, title: "Gérer", "aria-label": `Gérer ${u.username}` }, icon("edit")));
  }));
}

let managedUser = null;

// Comme Saga : un clic sur un utilisateur ouvre sa fiche, avec les actions possibles.
function openUserManage(u) {
  if (!u) return;
  managedUser = u;
  const self = u.id === state.me.id;
  $("#user-manage-title").textContent = u.username;
  const row = (title, desc, act, label, cls = "ghost") => el("div", { class: "list-row" },
    el("div", { class: "grow" }, el("strong", { text: title }), el("div", { class: "muted small", text: desc })),
    act ? el("button", { type: "button", class: `${cls} small`, "data-action": "user-act", "data-act": act, text: label }) : null);
  $("#user-manage-body").replaceChildren(el("div", { class: "list" },
    row("Rôle", u.is_admin ? "Administrateur : toutes les machines et l'administration." : "Utilisateur : machines attribuées seulement.",
      self ? null : u.is_admin ? "demote" : "promote", u.is_admin ? "Retirer les droits admin" : "Rendre administrateur"),
    row("Compte", u.disabled ? "Désactivé : connexion impossible." : "Actif.",
      self ? null : u.disabled ? "enable" : "disable", u.disabled ? "Réactiver" : "Désactiver", u.disabled ? "ghost" : "danger"),
    u.locked ? row("Verrouillage", "Trop d'échecs de connexion : compte verrouillé temporairement.", "unlock", "Déverrouiller") : null,
    row("Mot de passe", "Ses sessions en cours seront fermées.", "password", "Définir…"),
    row("Double authentification", u.mfa ? "Configurée." : "À configurer à la prochaine connexion.",
      u.mfa ? "reset-mfa" : null, "Réinitialiser", "danger")));
  $("#user-delete").hidden = self;
  const d = $("#user-manage-dialog");
  if (!d.open) d.showModal();
}

const USER_CONFIRM = {
  demote: (u) => `Retirer les droits administrateur de « ${u} » ?`,
  disable: (u) => `Désactiver « ${u} » ? Ses sessions en cours seront fermées.`,
  "reset-mfa": (u) => `Réinitialiser la double authentification de « ${u} » ? Il faudra la reconfigurer à la prochaine connexion.`,
  delete: (u) => `Supprimer définitivement « ${u} » ? Le journal d'audit conserve son historique.`,
};
const USER_DONE = {
  promote: "Droits administrateur accordés", demote: "Droits administrateur retirés", enable: "Compte réactivé",
  disable: "Compte désactivé", unlock: "Compte déverrouillé", "reset-mfa": "Double authentification réinitialisée", delete: "Utilisateur supprimé",
};

async function userAction(action) {
  const u = managedUser;
  if (!u) return;
  if (action === "password") return openPasswordDialog(u);
  if (USER_CONFIRM[action] && !confirm(USER_CONFIRM[action](u.username))) return;
  try {
    await api("POST", `/api/admin/users/${u.id}/${action}`, {});
    toast(USER_DONE[action] || "Enregistré", "ok");
    await loadPeople();
    renderUsers();
    renderGroups();
    const fresh = state.users.find((x) => x.id === u.id);
    if (fresh) openUserManage(fresh); else $("#user-manage-dialog").close();
  } catch (e) {
    handleAppError(e);
  }
}

$("#user-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const f = ev.target;
  formError(f, "");
  try {
    await api("POST", "/api/admin/users", {
      username: f.elements.username.value, password: f.elements.password.value, is_admin: f.elements.is_admin.checked,
    });
    $("#user-dialog").close();
    toast("Utilisateur créé", "ok");
    await loadPeople();
    renderUsers();
  } catch (e) {
    formError(f, e.message);
  }
});

// ---------- mot de passe défini par un administrateur ----------

let passwordTarget = null;

function openPasswordDialog(user) {
  passwordTarget = user;
  const f = $("#password-form");
  f.reset();
  formError(f, "");
  $("#password-title").textContent = `Mot de passe de « ${user.username} »`;
  $("#password-dialog").showModal();
  f.elements.new.focus();
}

$("#password-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const f = ev.target;
  formError(f, "");
  if (f.elements.new.value !== f.elements.confirm.value) return formError(f, "Les deux mots de passe ne correspondent pas.");
  try {
    await api("POST", `/api/admin/users/${passwordTarget.id}/password`, { password: f.elements.new.value });
    $("#password-dialog").close();
    toast("Mot de passe défini", "ok");
  } catch (e) {
    if (e.status === 401) { $("#password-dialog").close(); return boot(); }
    formError(f, e.message);
  }
});

// ---------- thème ----------

// Thèmes communs à Gná, Saga et Andvari (voir style.css). Les anciens noms sont convertis.
const THEMES = [
  { id: "auto", name: "Automatique", desc: "Gná ou Écume selon le système", sw: ["#0E1116", "#F6F8FA", "#5FB3A1"] },
  { id: "gna", name: "Gná", desc: "Sombre, bleu-vert de Gná", sw: ["#0E1116", "#161B22", "#5FB3A1"] },
  { id: "ecume", name: "Écume", desc: "Clair, bleu-vert", sw: ["#F6F8FA", "#FFFFFF", "#2F8F7B"] },
  { id: "fjord", name: "Fjord", desc: "Sombre, fjord bleu-vert", sw: ["#0B1418", "#122027", "#5FB3A1"] },
  { id: "saga", name: "Saga", desc: "Sombre, améthyste de Saga", sw: ["#0E1116", "#161B22", "#9B8AFB"] },
  { id: "velin", name: "Vélin", desc: "Clair, améthyste", sw: ["#F6F8FA", "#FFFFFF", "#6A55D9"] },
  { id: "nuit", name: "Nuit", desc: "Sombre, or d’Andvari", sw: ["#0E1116", "#161B22", "#E0B04A"] },
  { id: "parchemin", name: "Parchemin", desc: "Clair, or patiné", sw: ["#F6F8FA", "#FFFFFF", "#9C7019"] },
  { id: "braise", name: "Braise", desc: "Sombre, ambre de forge", sw: ["#120E0C", "#1C1613", "#E8834A"] },
  { id: "givre", name: "Givre", desc: "Clair, bleu glacier", sw: ["#F3F6FA", "#FFFFFF", "#2F6FB3"] },
];
const OLD_THEMES = { system: "auto", dark: "gna", light: "ecume", nord: "fjord", dracula: "saga", gruvbox: "braise", solarized: "parchemin" };
const LIGHT = new Set(["ecume", "velin", "parchemin", "givre"]);
const themeMedia = matchMedia("(prefers-color-scheme: light)");
let themeChoice = "auto";

const effectiveTheme = () => (themeChoice === "auto" ? (themeMedia.matches ? "ecume" : "gna") : themeChoice);

function applyTheme(name) {
  name = OLD_THEMES[name] || name;
  if (!THEMES.some((t) => t.id === name)) name = "auto";
  themeChoice = name;
  const id = effectiveTheme();
  document.documentElement.dataset.theme = id;
  const t = THEMES.find((x) => x.id === id);
  document.querySelector('meta[name="theme-color"]')?.setAttribute("content", t.sw[0]);
  try { localStorage.setItem("gna.theme", name); } catch { /* stockage indisponible */ }
  for (const b of $$("[data-theme-id]")) { b.classList.toggle("on", b.dataset.themeId === name); b.setAttribute("aria-checked", String(b.dataset.themeId === name)); }
  // Bouton de la barre du haut : soleil en sombre, lune en clair (comme Saga).
  $("#theme-toggle").replaceChildren(icon(LIGHT.has(id) ? "moon" : "sun", 18));
  window.dispatchEvent(new Event("gna-theme")); // terminaux et bureaux ouverts
}
themeMedia.addEventListener("change", () => themeChoice === "auto" && applyTheme("auto"));

// Le thème est enregistré dans le compte : il suit l'utilisateur d'un appareil à l'autre.
async function setTheme(theme) {
  applyTheme(theme);
  try {
    await api("PUT", "/api/me/prefs", { theme });
    if (state.me) state.me.prefs = { ...(state.me.prefs || {}), theme };
  } catch (e) {
    handleAppError(e);
  }
}

function renderThemePick() {
  const box = $("#theme-pick");
  box.replaceChildren(...THEMES.map((t) => {
    const sp = (cls, bg) => { const e = el("span", { class: cls }); e.style.background = bg; return e; };
    const pv = sp("pv", t.sw[0]);
    if (t.id === "auto") pv.append(sp("hf", t.sw[1]));
    else { const cd = sp("cd", t.sw[1]); cd.append(sp("br", t.sw[2])); pv.append(cd); }
    pv.append(sp("dt", t.sw[2]));
    return el("button", { type: "button", class: "opt", role: "radio", "data-theme-id": t.id },
      pv, el("span", { class: "tx" }, el("strong", { text: t.name }), el("span", { text: t.desc })));
  }));
  applyTheme(themeChoice);
}

document.addEventListener("click", (ev) => {
  const b = ev.target.closest("[data-theme-id]");
  if (b) setTheme(b.dataset.themeId);
});

// ---------- groupes ----------

function renderGroups() {
  const list = $("#groups");
  if (!state.groups.length) {
    list.replaceChildren(el("div", { class: "panel panel-pad muted small", text: "Aucun groupe pour l'instant." }));
    return;
  }
  list.replaceChildren(...state.groups.map((g) => el("div", { class: "list-row" },
    el("span", { class: "dot accent" }),
    el("div", { class: "grow" },
      el("div", { class: "line" }, el("strong", { text: g.name }),
        el("span", { class: "badge", text: plural(g.members.length, "membre") }),
        el("span", { class: "badge accent", text: plural(g.targets.length, "machine") })),
      g.description ? el("div", { class: "muted small", text: g.description }) : null),
    el("button", { type: "button", class: "icon-btn plain", "data-action": "edit-group", "data-id": g.id, title: "Modifier", "aria-label": `Modifier ${g.name}` }, icon("edit")))));
}

async function openGroupForm(g) {
  state.editingGroup = g || null;
  if (!state.targets.length) state.targets = await api("GET", "/api/targets");
  const f = $("#group-form");
  f.reset();
  formError(f, "");
  $("#group-title").textContent = g ? `Modifier « ${g.name} »` : "Ajouter un groupe";
  $("#group-delete").hidden = !g;
  if (g) {
    f.elements.name.value = g.name;
    f.elements.description.value = g.description;
  }
  renderChecklist($("#group-members"), state.users, g ? g.members : [], "member", "Aucun utilisateur",
    (u) => u.username + (u.is_admin ? " (admin)" : ""));
  renderChecklist($("#group-targets"), state.targets, g ? g.targets : [], "group_target", "Aucune machine",
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
    toast("Groupe enregistré", "ok");
    await loadPeople();
    renderGroups();
    renderUsers();
  } catch (e) {
    if (e.status === 401) { $("#group-dialog").close(); return boot(); }
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
    return el("button", { class: "ghost small", type: "button", "data-action": "replay", "data-id": s.id },
      icon("play", 13), `Rejouer · ${fmtSize(s.recording_size)}`);
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
  if (window.GnaVncReplay) window.GnaVncReplay.close();
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
          el("span", { class: "muted", text: `${fmtDate(r.started_at)} · ${plural(r.total, "résultat")}` })),
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
  if (state.me && state.page === "machines") loadTargets().catch(() => {});
});
