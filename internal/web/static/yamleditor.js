// Upgrade the YAML textarea to CodeMirror: syntax highlighting, line numbers,
// Tab/Shift-Tab indentation with two spaces. Without JS the textarea still works.
(function () {
  var ta = document.getElementById("yaml");
  if (!ta || !window.CodeMirror) return;
  var dark = window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches;
  var cm = CodeMirror.fromTextArea(ta, {
    mode: "yaml",
    lineNumbers: true,
    indentUnit: 2,
    tabSize: 2,
    indentWithTabs: false,
    lineWrapping: true,
    viewportMargin: Infinity,
    extraKeys: {
      Tab: function (cm) {
        if (cm.somethingSelected()) cm.indentSelection("add");
        else cm.replaceSelection("  ", "end");
      },
      "Shift-Tab": function (cm) { cm.indentSelection("subtract"); },
      "Ctrl-S": function () { document.getElementById("yamlform").requestSubmit(); },
      "Cmd-S": function () { document.getElementById("yamlform").requestSubmit(); }
    }
  });
  cm.getWrapperElement().classList.add(dark ? "cm-dark" : "cm-light");
  var err = document.querySelector(".flash.err pre");
  var m = err && /line (\d+)/.exec(err.textContent);
  if (m) {
    var line = parseInt(m[1], 10) - 1;
    cm.addLineClass(line, "background", "cm-error-line");
    cm.scrollIntoView({ line: line, ch: 0 }, 120);
    cm.setCursor({ line: line, ch: 0 });
  }
  cm.focus();
})();
