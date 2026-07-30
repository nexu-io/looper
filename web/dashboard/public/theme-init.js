// CSP-safe theme bootstrap (script-src 'self'). Runs before paint so the
// first frame matches the persisted preference. CSS falls back to the
// media query when no attribute is set, so failures still look sensible.
(function () {
  try {
    var m = localStorage.getItem("looper.dashboard.theme");
    if (m !== "light" && m !== "dark" && m !== "system") m = "system";
    document.documentElement.setAttribute("data-theme", m);
  } catch (_) {
    /* localStorage unavailable — leave attribute unset, CSS media query decides. */
  }
})();
