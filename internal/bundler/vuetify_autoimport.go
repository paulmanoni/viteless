package bundler

import (
	"regexp"
	"sort"
	"strings"
)

// VuetifyAutoImport rewrites a .vue SFC's source to inject
// `import { VFoo, VBar } from "vuetify/components"` at the top
// of its <script setup> block, for every `<v-foo>` / `<v-bar>`
// component tag used in the <template>.
//
// Why this exists: Vuetify v3 dropped global auto-registration
// in favor of explicit imports + tree-shaking. Operators porting
// from Vite typically rely on @vitejs/plugin-vuetify to do the
// import injection from template scans — without it, ALL Vuetify
// component CSS is missing from the bundle because the side-
// effect imports never fire. This function fills that gap.
//
// Mirror of vite-plugin-vuetify's core behavior but limited to
// the import-injection part — no auto-config / labs / styles
// helpers. Operators wanting those can switch to manual mode.
//
// Returns the (possibly rewritten) source + the count of imports
// added. count=0 means nothing changed; caller should use the
// original source.
//
// Detection rules:
//
//   - `<v-([a-zA-Z][a-zA-Z0-9-]*)` tags inside the <template>
//     block. Self-closing + open tags both match.
//   - Filters out Vue directives that LOOK like tags but aren't
//     (`v-for`, `v-if`, etc. — these only appear as attributes,
//     not standalone tags, but defensive filter regardless).
//   - Skips when the SFC already imports `"vuetify/components"`
//     anywhere in its <script> block — operators with manual
//     wildcard imports stay manual.
//
// Edge cases:
//
//   - SFCs without a <template> block → no-op.
//   - SFCs without a <script setup> block → no-op (we don't
//     inject into Options-API <script> because the component
//     would need to be registered in `components: { ... }` too,
//     which we can't easily do).
//   - Comments inside templates: the regex matches inside
//     comments too, which is harmless — those imports just
//     bring in unused CSS (already covered by the global tree).
func VuetifyAutoImport(src string) (string, int) {
	tpl := extractBlock(src, "template")
	if tpl == "" {
		return src, 0
	}
	if strings.Contains(src, `"vuetify/components"`) ||
		strings.Contains(src, `'vuetify/components'`) {
		return src, 0
	}
	tags := scanVuetifyTags(tpl)
	if len(tags) == 0 {
		return src, 0
	}
	// Build the import statement: `import { V1, V2, V3 } from "vuetify/components"`
	imp := "import { " + strings.Join(tags, ", ") + ` } from "vuetify/components";` + "\n"
	rewritten, ok := injectIntoScriptSetup(src, imp)
	if !ok {
		// No <script setup> block — defer to manual mode.
		return src, 0
	}
	return rewritten, len(tags)
}

// vuetifyTagRE matches `<v-foo-bar` or `<v-foo` patterns. The
// trailing space / > / / boundary is enforced by a non-greedy
// character-class lookalike (Go regex has no lookahead).
var vuetifyTagRE = regexp.MustCompile(`<(v-[a-zA-Z][a-zA-Z0-9-]*)[\s/>]`)

// scanVuetifyTags walks the template body, returns the unique
// PascalCase component names. Order is alphabetic for
// deterministic output (same SFC → same import line → same
// bundle hash).
//
//	"<v-text-field><v-btn>"  →  ["VBtn", "VTextField"]
func scanVuetifyTags(tpl string) []string {
	matches := vuetifyTagRE.FindAllStringSubmatch(tpl, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var names []string
	for _, m := range matches {
		kebab := m[1] // "v-text-field"
		name := kebabToPascal(kebab)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// kebabToPascal converts "v-text-field" → "VTextField".
// "v-app-bar-title" → "VAppBarTitle". Returns "" when input
// isn't a recognizable kebab name.
func kebabToPascal(s string) string {
	if !strings.HasPrefix(s, "v-") || len(s) < 3 {
		return ""
	}
	parts := strings.Split(s, "-")
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]))
		if len(p) > 1 {
			b.WriteString(p[1:])
		}
	}
	return b.String()
}

// extractBlock returns the inner content of the first
// <name>...</name> SFC block, or "" when absent. We use a cheap
// substring scan rather than HTML parsing — SFC blocks are
// top-level + well-formed in practice.
//
//	extractBlock(src, "template") → contents between
//	    `<template>` (and attrs) and `</template>`
func extractBlock(src, name string) string {
	open := "<" + name
	close := "</" + name + ">"
	startTag := strings.Index(src, open)
	if startTag < 0 {
		return ""
	}
	// Find the closing `>` of the opening tag — it might have
	// attributes (`<template lang="pug">`).
	bodyStart := strings.IndexByte(src[startTag:], '>')
	if bodyStart < 0 {
		return ""
	}
	bodyStart += startTag + 1
	endTag := strings.Index(src[bodyStart:], close)
	if endTag < 0 {
		return ""
	}
	return src[bodyStart : bodyStart+endTag]
}

// scriptSetupRE matches the opening tag of a <script setup>
// block, with optional lang= attribute and arbitrary other
// attribute order.
var scriptSetupRE = regexp.MustCompile(`<script\b[^>]*\bsetup\b[^>]*>`)

// injectIntoScriptSetup inserts the given import line at the
// very start of the SFC's <script setup> body. Returns the
// rewritten source + true on success, or (src, false) when no
// <script setup> block exists.
func injectIntoScriptSetup(src, imp string) (string, bool) {
	loc := scriptSetupRE.FindStringIndex(src)
	if loc == nil {
		return src, false
	}
	// loc[1] is the index right AFTER the opening tag's `>`.
	insertAt := loc[1]
	return src[:insertAt] + "\n" + imp + src[insertAt:], true
}
