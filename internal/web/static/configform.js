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
  // "Test" sends only this row's fields; the server tests them without saving.
  function testRow(btn) {
    var row = btn.closest("[data-row]");
    var out = row.querySelector(".test-result");
    var params = new URLSearchParams();
    row.querySelectorAll("input[name], select[name]").forEach(function (el) {
      if (el.type === "checkbox") { if (el.checked) params.append(el.name, "on"); }
      else params.append(el.name, el.value);
    });
    btn.disabled = true;
    out.className = "test-result muted";
    out.textContent = "Testing…";
    fetch(form.getAttribute("data-test-url"), {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body: params.toString(),
      credentials: "same-origin"
    }).then(function (r) { return r.json(); }).then(function (res) {
      out.className = "test-result " + (res.ok ? "ok" : "err");
      out.textContent = (res.ok ? "✓ " : "✗ ") + (res.message || res.error || "unknown result");
    }).catch(function (e) {
      out.className = "test-result err";
      out.textContent = "✗ " + e;
    }).finally(function () { btn.disabled = false; });
  }
  // Show username/password or API key fields for the selected auth type.
  function syncAuth(select) {
    var row = select.closest("[data-row]");
    row.querySelectorAll("[data-auth]").forEach(function (el) {
      el.hidden = el.getAttribute("data-auth") !== select.value;
    });
  }
  form.addEventListener("change", function (ev) {
    if (ev.target.matches("[data-auth-select]")) syncAuth(ev.target);
  });
  form.querySelectorAll("[data-auth-select]").forEach(syncAuth);

  form.addEventListener("click", function (ev) {
    var test = ev.target.closest("[data-test]");
    if (test) { testRow(test); return; }
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
