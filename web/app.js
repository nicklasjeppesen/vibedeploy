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
