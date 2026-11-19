// Gná — thème, menu mobile, boutons « Copier »
(function () {
  var root = document.documentElement;

  // Thème : clair / sombre, mémorisé dans le navigateur
  var toggle = document.getElementById('theme-toggle');
  function current() {
    if (root.dataset.theme) return root.dataset.theme;
    return window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark';
  }
  toggle.addEventListener('click', function () {
    var next = current() === 'light' ? 'dark' : 'light';
    root.dataset.theme = next;
    try { localStorage.setItem('gna-theme', next); } catch (e) {}
  });

  // Menu mobile
  var menu = document.getElementById('menu-toggle');
  var nav = document.getElementById('nav');
  menu.addEventListener('click', function () {
    var open = nav.classList.toggle('open');
    menu.setAttribute('aria-expanded', open ? 'true' : 'false');
  });
  nav.addEventListener('click', function (e) {
    if (e.target.tagName === 'A') {
      nav.classList.remove('open');
      menu.setAttribute('aria-expanded', 'false');
    }
  });

  // Documentation : sommaire replié sur mobile, section en cours surlignée
  var toc = document.getElementById('toc');
  if (toc) {
    var narrow = window.matchMedia('(max-width: 980px)');
    if (narrow.matches) toc.open = false;
    var links = toc.querySelectorAll('a[href^="#"]');
    var byId = {};
    links.forEach(function (a) {
      byId[a.getAttribute('href').slice(1)] = a;
      a.addEventListener('click', function () { if (narrow.matches) toc.open = false; });
    });
    var sections = document.querySelectorAll('.doc section[id]');
    var onScroll = function () {
      var current = sections[0];
      sections.forEach(function (s) { if (s.getBoundingClientRect().top < 140) current = s; });
      links.forEach(function (a) { a.classList.remove('active'); });
      if (current && byId[current.id]) byId[current.id].classList.add('active');
    };
    window.addEventListener('scroll', onScroll, { passive: true });
    onScroll();
  }

  // Copier les commandes
  document.querySelectorAll('.copy').forEach(function (btn) {
    btn.addEventListener('click', function () {
      var text = btn.getAttribute('data-copy');
      var done = function () {
        btn.textContent = 'Copié';
        setTimeout(function () { btn.textContent = 'Copier'; }, 1600);
      };
      if (navigator.clipboard && window.isSecureContext) {
        navigator.clipboard.writeText(text).then(done);
      } else {
        var ta = document.createElement('textarea');
        ta.value = text;
        document.body.appendChild(ta);
        ta.select();
        try { document.execCommand('copy'); done(); } catch (e) {}
        ta.remove();
      }
    });
  });
})();
