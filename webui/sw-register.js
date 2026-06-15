(function () {
  if (!('serviceWorker' in navigator)) return;

  const base = typeof BASE_URL !== 'undefined' ? BASE_URL : '';
  const scope = (base || '/') + (base && !base.endsWith('/') ? '/' : '');
  const scriptURL = (base || '') + '/sw.js';

  window.addEventListener('load', function () {
    navigator.serviceWorker.register(scriptURL, { scope }).catch(function () {
      // Keep the existing page behavior if the offline fallback cannot register.
    });
  });
})();
