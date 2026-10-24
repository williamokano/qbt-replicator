// Apply torrent filters as you type (debounced) or change a dropdown.
(function () {
  var form = document.getElementById("torrent-filters");
  if (!form) return;
  var timer;
  var submit = function () { form.requestSubmit ? form.requestSubmit() : form.submit(); };
  form.querySelectorAll("select, input[type=checkbox]").forEach(function (el) {
    el.addEventListener("change", submit);
  });
  var search = form.querySelector("input[type=search]");
  if (search) {
    search.addEventListener("input", function () {
      clearTimeout(timer);
      timer = setTimeout(submit, 400);
    });
    if (search.value) { // keep typing after the reload
      search.focus();
      search.setSelectionRange(search.value.length, search.value.length);
    }
  }
})();
