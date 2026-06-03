// adapter.js — bridge between Go-side compile.go and the real
// @vue/compiler-sfc package fetched from esm.sh.
//
// At bootstrap time esbuild bundles THIS file + @vue/compiler-sfc
// (plus the transitive deps esm.sh stitches in) into one IIFE.
// QuickJS evaluates the IIFE once, installing globalThis.
// __nexus_compileSFC. compile.go then invokes the global for
// every .vue file the bundler sees.
//
// Contract:
//
//	{ code: string, errors: [{message, line?, column?}] }
//
// compile.go does NOT depend on any Vue-specific shape; this
// adapter is the only place Vue-aware code lives.

import * as compiler from "@vue/compiler-sfc";

(function () {
    // Stable hashed scope id from filename + source. Avoids
    // needing a crypto dep in the runtime — Vue only requires
    // the id be unique per component instance, not
    // cryptographically opaque. Folding the source in (not just
    // the path) means two files whose paths happen to collide
    // under this cheap hash still get distinct scope ids, so
    // their `scoped` CSS can't cross-contaminate.
    function scopeId(filename, source) {
        var h = 0, str = filename + "\0" + source;
        for (var i = 0; i < str.length; i++) {
            h = ((h << 5) - h + str.charCodeAt(i)) | 0;
        }
        return "data-v-" + (h >>> 0).toString(36);
    }

    function safeMessage(e) {
        if (!e) return "unknown error";
        if (typeof e === "string") return e;
        return e.message || String(e);
    }

    function locOf(e) {
        var loc = e && e.loc && e.loc.start;
        return {
            line: loc ? loc.line : 0,
            column: loc ? loc.column : 0,
        };
    }

    // Escape a literal segment for embedding inside a backtick
    // template literal: backslash, backtick, and `$` (so a stray
    // "${" in the CSS can't open an interpolation).
    function escTemplate(s) {
        return s
            .replace(/\\/g, "\\\\")
            .replace(/`/g, "\\`")
            .replace(/\$/g, "\\$");
    }

    // Rewrite CSS url() references into bundler-resolvable ESM
    // imports — the same trick @vue/compiler-sfc's template pass
    // uses for `src="@/..."`. compileStyle only scopes selectors;
    // it leaves url() verbatim (URL resolution is the bundler's
    // job). Since we inject CSS as a runtime string rather than a
    // real CSS module, esbuild never sees those url() tokens, so a
    // bare `url('@/assets/x.png')` would ship as-is and 404.
    //
    // Here we hoist each resolvable url() to `import __nl_url_N
    // from "<spec>"` (esbuild resolves it via tsconfig paths +
    // the file loader → hashed public URL) and rebuild the CSS as
    // a template literal interpolating those bindings. The result
    // matches Vite: relative + aliased assets resolve and hash;
    // absolute/external/data/fragment URLs pass through untouched.
    //
    // Returns { imports: [string], expr: string } where expr is a
    // JS template-literal expression (backticks included).
    function buildCssModule(css, startIdx) {
        var imports = [];
        var re = /url\(\s*(?:'([^']*)'|"([^"]*)"|([^)'"\s]+))\s*\)/g;
        var out = "`";
        var last = 0;
        var idx = startIdx;
        var m;
        while ((m = re.exec(css)) !== null) {
            var raw = m[1] != null ? m[1] : (m[2] != null ? m[2] : m[3]);
            var spec = raw.trim();
            out += escTemplate(css.slice(last, m.index));
            last = re.lastIndex;

            // Pass through anything the bundler shouldn't resolve:
            // empty, protocol/protocol-relative, root-absolute,
            // fragment-only, and data: URIs.
            if (
                spec === "" ||
                /^(?:[a-z]+:)?\/\//i.test(spec) ||
                spec.charAt(0) === "/" ||
                spec.charAt(0) === "#" ||
                spec.lastIndexOf("data:", 0) === 0
            ) {
                out += escTemplate(m[0]);
                continue;
            }

            // Split off any ?query / #hash so the import specifier
            // is clean, then re-append it to the resolved URL.
            var suffix = "";
            var importPath = spec;
            var qh = spec.search(/[?#]/);
            if (qh !== -1) {
                suffix = spec.slice(qh);
                importPath = spec.slice(0, qh);
            }

            var v = "__nl_url_" + idx;
            idx++;
            imports.push("import " + v + " from " + JSON.stringify(importPath) + ";");
            out += 'url("${' + v + '}' + escTemplate(suffix) + '")';
        }
        out += escTemplate(css.slice(last));
        out += "`";
        return { imports: imports, expr: out, next: idx };
    }

    globalThis.__nexus_compileSFC = function (source, filename) {
        try {
            var parsed = compiler.parse(source, { filename: filename });
            var descriptor = parsed.descriptor;
            if (parsed.errors && parsed.errors.length) {
                return {
                    code: "",
                    errors: parsed.errors.map(function (e) {
                        var l = locOf(e);
                        return { message: safeMessage(e), line: l.line, column: l.column };
                    }),
                };
            }

            // Loud guards for SFC features this synchronous adapter
            // can't honor — fail with a clear message rather than
            // emitting silently-wrong output. compileStyle /
            // compileTemplate run synchronously here, so anything
            // needing a preprocessor (scss/less/pug/...) or a
            // post-parse binding injection (CSS Modules' $style) is
            // out of scope until the async path lands.
            var guardErrors = [];
            for (var gi = 0; gi < descriptor.styles.length; gi++) {
                var gst = descriptor.styles[gi];
                if (gst.module) {
                    guardErrors.push({
                        message: "<style module> is not supported (" + filename + ")",
                        line: 0, column: 0,
                    });
                }
                var slang = gst.lang ? String(gst.lang).toLowerCase() : "";
                if (slang && slang !== "css") {
                    guardErrors.push({
                        message: "<style lang=\"" + gst.lang + "\"> requires a preprocessor, which is not supported (" + filename + ")",
                        line: 0, column: 0,
                    });
                }
            }
            if (descriptor.template && descriptor.template.lang) {
                var tlang = String(descriptor.template.lang).toLowerCase();
                if (tlang && tlang !== "html") {
                    guardErrors.push({
                        message: "<template lang=\"" + descriptor.template.lang + "\"> requires a preprocessor, which is not supported (" + filename + ")",
                        line: 0, column: 0,
                    });
                }
            }
            if (guardErrors.length) {
                return { code: "", errors: guardErrors };
            }

            var id = scopeId(filename);
            var hasScoped = descriptor.styles.some(function (s) { return s.scoped; });
            var allErrors = [];
            var assembled = "";

            // --- script / scriptSetup ---------------------------------------
            // compileScript handles both classic <script> and
            // <script setup>, inlining the template when scriptSetup
            // is present (Vue's recommended path — produces fewer
            // indirections than the template-as-render-fn branch).
            var scriptResult = null;
            if (descriptor.script || descriptor.scriptSetup) {
                try {
                    scriptResult = compiler.compileScript(descriptor, {
                        id: id,
                        inlineTemplate: !!descriptor.scriptSetup,
                        templateOptions: {
                            id: id,
                            scoped: hasScoped,
                        },
                    });
                    // compileScript emits an ESM. Rewrite the
                    // "export default" into an assignment so the
                    // module assembly below can attach render + __file.
                    var content = scriptResult.content.replace(
                        /export\s+default\s+/,
                        "const __sfc__ = "
                    );
                    assembled += content + "\n";
                } catch (e) {
                    var l = locOf(e);
                    allErrors.push({ message: safeMessage(e), line: l.line, column: l.column });
                }
            } else {
                assembled += "const __sfc__ = {};\n";
            }

            // --- template (only when scriptSetup didn't inline it) ----------
            if (descriptor.template && !descriptor.scriptSetup) {
                try {
                    var tplResult = compiler.compileTemplate({
                        source: descriptor.template.content,
                        filename: filename,
                        id: id,
                        scoped: hasScoped,
                        compilerOptions: { hoistStatic: true },
                    });
                    if (tplResult.errors && tplResult.errors.length) {
                        for (var i = 0; i < tplResult.errors.length; i++) {
                            var l2 = locOf(tplResult.errors[i]);
                            allErrors.push({
                                message: safeMessage(tplResult.errors[i]),
                                line: l2.line, column: l2.column,
                            });
                        }
                    }
                    assembled += tplResult.code + "\n";
                    assembled += "__sfc__.render = render;\n";
                } catch (e) {
                    var l3 = locOf(e);
                    allErrors.push({ message: safeMessage(e), line: l3.line, column: l3.column });
                }
            }

            // --- styles -----------------------------------------------------
            // Inline scoped styles as a one-shot
            // document.head.appendChild. CSS extraction + a real
            // sidecar bundle is a v0.2 concern.
            var cssChunks = [];
            for (var k = 0; k < descriptor.styles.length; k++) {
                var st = descriptor.styles[k];
                try {
                    var styleResult = compiler.compileStyle({
                        source: st.content,
                        filename: filename,
                        id: id,
                        scoped: !!st.scoped,
                    });
                    if (styleResult.errors && styleResult.errors.length) {
                        for (var j = 0; j < styleResult.errors.length; j++) {
                            allErrors.push({
                                message: safeMessage(styleResult.errors[j]),
                                line: 0, column: 0,
                            });
                        }
                    }
                    cssChunks.push(styleResult.code);
                } catch (e) {
                    allErrors.push({ message: safeMessage(e), line: 0, column: 0 });
                }
            }
            if (cssChunks.length) {
                var css = cssChunks.join("\n");
                // Hoist url() refs to ESM imports so esbuild resolves
                // + hashes them (Vite-equivalent behavior), then inject
                // the interpolated CSS at runtime. Imports must sit at
                // module top; esbuild hoists them regardless of order.
                var cssMod = buildCssModule(css, 0);
                var cssImports = cssMod.imports.length
                    ? cssMod.imports.join("\n") + "\n"
                    : "";
                assembled =
                    cssImports +
                    "const __css = " + cssMod.expr + ";\n" +
                    "if (typeof document !== 'undefined') {\n" +
                    "  const __s = document.createElement('style');\n" +
                    "  __s.setAttribute('data-nl-sfc', " + JSON.stringify(id) + ");\n" +
                    "  __s.textContent = __css;\n" +
                    "  document.head.appendChild(__s);\n" +
                    "}\n" + assembled;
            }

            assembled +=
                "__sfc__.__file = " + JSON.stringify(filename) + ";\n" +
                "__sfc__.__scopeId = " + JSON.stringify(id) + ";\n";

            // --- HMR registration (dev only, at RUNTIME) --------------------
            // Stamp __hmrId and register the component with Vue's HMR
            // runtime so a later hot update can target it. The block is
            // gated on `globalThis.__VUE_HMR_RUNTIME__` being present —
            // the dev Vue build installs it as `getGlobalThis()
            // .__VUE_HMR_RUNTIME__` (a property on the global object, NOT
            // a bare binding), so it must be read off globalThis. It's
            // present only in the development build (nexus dev swaps
            // vue→vue.development.mjs); in production it's undefined, so
            // this is a dead no-op and the prod bundle is unaffected — no
            // compile-time dev flag needed.
            //
            // createRecord(id, component) is idempotent per id; the scope
            // id is path-stable, so re-running this module (or compiling
            // the same file again) keeps the same record.
            assembled +=
                "__sfc__.__hmrId = " + JSON.stringify(id) + ";\n" +
                "if (typeof globalThis !== 'undefined' && globalThis.__VUE_HMR_RUNTIME__) {\n" +
                "  globalThis.__VUE_HMR_RUNTIME__.createRecord(__sfc__.__hmrId, __sfc__);\n" +
                "}\n";

            assembled += "export default __sfc__;\n";

            return { code: assembled, errors: allErrors };
        } catch (e) {
            return {
                code: "",
                errors: [{ message: "adapter crashed: " + safeMessage(e), line: 0, column: 0 }],
            };
        }
    };
})();
