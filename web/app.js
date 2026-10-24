document.addEventListener('click', function (e) {
  var b = e.target.closest('[data-copy]');
  if (!b) return;
  var el = document.getElementById(b.getAttribute('data-copy'));
  if (!el) return;
  navigator.clipboard.writeText(el.textContent).then(function () {
    var old = b.textContent;
    b.textContent = 'Kopieret';
    setTimeout(function () { b.textContent = old; }, 1500);
  });
});

// En dropdown med «Andet …» (data-other="<id>") viser feltet med det id kun, når «Andet …» er valgt.
// Uden JavaScript står feltet altid fremme, så formularen stadig virker.
document.querySelectorAll('select[data-other]').forEach(function (s) {
  var box = document.getElementById(s.getAttribute('data-other'));
  if (!box) return;
  var sync = function () { box.hidden = s.value !== 'other'; };
  s.addEventListener('change', sync);
  sync();
});

// Appens side følger med live (Server-Sent Events fra /apps/<slug>/events): status og tekst opdateres, og
// nye loglinjer kommer på loggen, uden at siden genindlæses. Når appen er færdig (live, fejlet, stoppet eller
// en ny version), genindlæses siden én gang, så knapperne passer. Har man skrevet i en formular, genindlæses
// den ikke; i stedet vises en knap, så intet går tabt.
(function () {
  var root = document.querySelector('[data-live]');
  if (!root || !window.EventSource) return;
  var startStatus = root.getAttribute('data-status');
  var startCurrent = root.getAttribute('data-current');
  var dirty = false;
  document.addEventListener('input', function (e) { if (e.target.closest('form')) dirty = true; });
  var $ = function (id) { return document.getElementById(id); };
  // Genindlæs uden ?m=… i adressen, så en gammel besked («Godkendt. Appen bygges nu.») ikke bliver stående.
  var reload = function () {
    var u = new URL(location.href);
    u.searchParams.delete('m');
    location.replace(u.toString());
  };
  var done = function (text) {
    if (!dirty) { reload(); return; }
    $('live-done-text').textContent = text + ' Du har skrevet noget på siden, så den genindlæses ikke af sig selv.';
    $('live-done').hidden = false;
  };
  if ($('live-reload')) $('live-reload').addEventListener('click', reload);

  var es = new EventSource(root.getAttribute('data-live'));
  es.addEventListener('status', function (e) {
    var s = JSON.parse(e.data);
    var badge = $('live-status');
    if (badge) badge.className = 'status ' + s.class;
    if ($('live-label')) $('live-label').textContent = s.label;
    if ($('live-busy')) {
      $('live-busy').hidden = !s.busy;
      $('live-text').textContent = s.text || (s.busy ? 'Arbejder …' : '');
    }
    var changed = s.status !== startStatus || String(s.current) !== startCurrent;
    if (changed && !s.busy) {
      es.close();
      done(s.status === 'live' ? 'Appen er live.' : 'Appens status er nu: ' + s.label + '.');
    }
    if (s.busy) startStatus = s.status; // fra «bygger» til «starter» er ikke færdig endnu
  });
  es.addEventListener('log', function (e) {
    var pre = $('live-log');
    if (!pre) return;
    var atBottom = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 20;
    if (pre.textContent === '(ingen log endnu)') pre.textContent = '';
    pre.textContent += (pre.textContent ? '\n' : '') + e.data;
    if (atBottom) pre.scrollTop = pre.scrollHeight; // rul kun med, hvis man stod nederst
  });
  es.addEventListener('gone', function () { es.close(); location.href = '/'; });
})();

// Forsiden følger med live (/events): når en app skifter status, hentes forsiden igen i baggrunden, og
// indholdet skiftes ud uden at siden genindlæses eller hopper.
(function () {
  var root = document.querySelector('[data-home-live]');
  if (!root || !window.EventSource) return;
  var timer = null, busy = false;
  var refresh = function () {
    if (busy) { timer = setTimeout(refresh, 300); return; }
    busy = true;
    fetch(location.pathname, { credentials: 'same-origin', headers: { 'Accept': 'text/html' } })
      .then(function (r) { return r.ok ? r.text() : Promise.reject(r.status); })
      .then(function (html) {
        var next = new DOMParser().parseFromString(html, 'text/html').getElementById('home-live');
        var cur = document.getElementById('home-live');
        if (next && cur) cur.replaceWith(next);
      })
      .catch(function () {})
      .then(function () { busy = false; });
  };
  var es = new EventSource(root.getAttribute('data-home-live'));
  es.addEventListener('changed', function () {
    clearTimeout(timer);
    timer = setTimeout(refresh, 200); // saml flere ændringer tæt på hinanden til én hentning
  });
})();
