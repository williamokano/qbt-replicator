// Add/remove instance and rule rows in the config form. Row inputs are named
// "<kind>-<index>-<field>"; new rows get an index above every existing one.
(function () {
  var form = document.getElementById("cfgform");
  if (!form) return;
  var next = 0;
  form.querySelectorAll("[name]").forEach(function (el) {
    var m = /^(?:inst|rule)-(\d+)-/.exec(el.name);
    if (m) next = Math.max(next, parseInt(m[1], 10) + 1);
  });
  form.addEventListener("click", function (ev) {
    var add = ev.target.closest("[data-add]");
    if (add) {
      var kind = add.getAttribute("data-add");
      var tpl = document.getElementById("tpl-" + kind);
      var holder = document.createElement("div");
      holder.innerHTML = tpl.innerHTML.replace(/__i__/g, String(next++));
      var row = holder.firstElementChild;
      document.getElementById(kind + "-rows").appendChild(row);
      var first = row.querySelector("input:not([type=hidden])");
      if (first) first.focus();
      return;
    }
    var rm = ev.target.closest("[data-remove]");
    if (rm && confirm("Remove this entry? (Nothing changes until you save.)")) {
      rm.closest("[data-row]").remove();
    }
  });
})();
