// Thème appliqué avant l'affichage (évite un flash de l'ancien thème). Le choix est
// enregistré sur le serveur (préférences du compte) et recopié ici pour le chargement suivant.
try {
  const old = { system: "auto", dark: "gna", light: "ecume", nord: "fjord", dracula: "saga", gruvbox: "braise", solarized: "parchemin" };
  let t = localStorage.getItem("gna.theme") || "auto";
  t = old[t] || t;
  if (t === "auto") t = matchMedia("(prefers-color-scheme: light)").matches ? "ecume" : "gna";
  document.documentElement.dataset.theme = t;
} catch { /* stockage indisponible */ }
// Application Gná (relais local sur 127.0.0.1) : WebKitGTK peine avec le flou d'arrière-plan
// et les animations permanentes ; style.css les retire sous html.app.
if (location.hostname === "127.0.0.1") document.documentElement.classList.add("app");
