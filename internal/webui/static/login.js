// Sign the console in with the one-time code in the URL's fragment, which
// `wayfindr ui` put there. The fragment never reaches the server in a
// request line; this posts it once and drops it from the address bar.
(function () {
  const code = location.hash.slice(1);

  if (!code) {
    return;
  }

  history.replaceState(null, "", location.pathname);
  const form = document.getElementById("login");
  form.elements.code.value = code;
  form.submit();
})();
