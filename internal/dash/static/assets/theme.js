// The theme, set before the page paints: Dim unless this browser chose another. A classic script
// in <head>, not a module, so it runs first; a file, not inline, so the CSP allows it. The switch
// is on the Settings page (settings.js). Storage can be missing or throw (a private window), hence the try.
(function () {
  try {
    var t = localStorage.getItem("agent-box:theme");
    if (t === "dark" || t === "dim" || t === "light") document.documentElement.dataset.theme = t;
  } catch (e) {}
})();
