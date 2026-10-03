// Applies the saved look before the page is drawn (review #31): a plain script, run in <head>
// after the stylesheet. The settings page changes it through theme.js.
(function () {
  var p = {};
  try {
    p = JSON.parse(localStorage.getItem('kanade.theme') || '{}') || {};
  } catch (e) { /* no storage: the default look */ }
  var d = document.documentElement;
  if (p.theme === 'teal' || p.theme === 'rose') d.setAttribute('data-theme', p.theme);
  if (p.mode === 'light' || p.mode === 'dark') d.setAttribute('data-mode', p.mode);
  var bars = { violet: ['#fbf8ff', '#121318'], teal: ['#f4fbfa', '#0e1515'], rose: ['#fff8f7', '#1a1111'] }[p.theme] || ['#fbf8ff', '#121318'];
  var metas = document.querySelectorAll('meta[name="theme-color"]');
  for (var i = 0; i < metas.length; i++) {
    var dark = /dark/.test(metas[i].media);
    metas[i].content = bars[p.mode === 'dark' || (p.mode !== 'light' && dark) ? 1 : 0];
  }
})();
