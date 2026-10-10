// Thème appliqué avant l'affichage (évite un flash de l'ancien thème). Le choix est
// enregistré sur le serveur (préférences du compte) et recopié ici pour le chargement suivant.
try {
  const t = localStorage.getItem("gna.theme");
  if (t) document.documentElement.dataset.theme = t;
} catch { /* stockage indisponible */ }
