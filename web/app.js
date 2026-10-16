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
