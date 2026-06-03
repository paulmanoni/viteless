// fake-adapter.js — minimal stand-in for the real @vue/compiler-sfc
// adapter the production code loads. Used by unit tests so the
// Go ↔ QuickJS plumbing can be exercised without pulling MB of
// real Vue compiler bytes.
//
// Contract matches compile.go's expectations:
//
//   globalThis.__nexus_compileSFC(source, filename)
//     → { code: string, errors?: [{message, line?, column?}] }
//
// Deterministic behavior so tests can assert exact output:
//   - Extracts the <template> body and emits it as a JS string
//     in a default-export.
//   - Source starting with the literal "BOOM" returns one error
//     to exercise the errors path.

(function () {
    globalThis.__nexus_compileSFC = function (source, filename) {
        if (source.indexOf("BOOM") === 0) {
            return {
                code: "",
                errors: [
                    { message: "synthetic test error", line: 1, column: 1 },
                ],
            };
        }
        var m = source.match(/<template>([\s\S]*?)<\/template>/);
        var tmpl = m ? m[1].trim() : "";
        return {
            code:
                "export default { template: " +
                JSON.stringify(tmpl) +
                ", __filename: " +
                JSON.stringify(filename) +
                " };",
        };
    };
})();
