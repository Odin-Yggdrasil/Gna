// Applique le thème mémorisé avant l'affichage (évite un flash de l'autre thème).
// Fichier séparé : la politique de sécurité du site (CSP) interdit les scripts inline.
try {
  var t = localStorage.getItem('gna-theme');
  if (t === 'light' || t === 'dark') document.documentElement.dataset.theme = t;
} catch (e) {}
