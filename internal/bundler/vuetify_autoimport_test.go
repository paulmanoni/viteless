package bundler

import (
	"strings"
	"testing"
)

// TestVuetifyAutoImport_BasicTemplate: the headline case. A
// minimal Vuetify SFC with v-text-field + v-btn in the template
// gets `import { VBtn, VTextField } from "vuetify/components"`
// injected at the top of <script setup>.
func TestVuetifyAutoImport_BasicTemplate(t *testing.T) {
	src := `
<template>
  <v-form>
    <v-text-field label="Name" />
    <v-btn>Submit</v-btn>
  </v-form>
</template>

<script setup lang="ts">
import { ref } from "vue";
const name = ref("");
</script>
`
	got, n := VuetifyAutoImport(src)
	if n == 0 {
		t.Fatal("expected imports to be added")
	}
	for _, want := range []string{"VBtn", "VForm", "VTextField"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in import line:\n%s", want, got)
		}
	}
	// The injection point must be RIGHT after `<script setup ...>`
	// — otherwise Vue's compiler-sfc won't see it as part of
	// the script setup body.
	scriptSetupIdx := strings.Index(got, "<script setup")
	closeIdx := strings.Index(got[scriptSetupIdx:], ">")
	if closeIdx < 0 {
		t.Fatal("malformed output: <script setup not closed")
	}
	bodyStart := scriptSetupIdx + closeIdx + 1
	body := got[bodyStart:]
	if !strings.HasPrefix(strings.TrimLeft(body, "\n\t "), "import { VBtn, VForm, VTextField }") {
		t.Errorf("import not at top of <script setup>:\n%s", body)
	}
}

// TestVuetifyAutoImport_AlreadyManualSkips: when the operator
// already does `import * as components from "vuetify/components"`
// or any other explicit vuetify/components import, the plugin
// stays out of their way.
func TestVuetifyAutoImport_AlreadyManualSkips(t *testing.T) {
	src := `
<template>
  <v-btn>Click</v-btn>
</template>

<script setup lang="ts">
import * as components from "vuetify/components";
</script>
`
	got, n := VuetifyAutoImport(src)
	if n != 0 {
		t.Errorf("expected no-op when manual import present; injected %d", n)
	}
	if got != src {
		t.Errorf("source mutated despite no-op:\n%s", got)
	}
}

// TestVuetifyAutoImport_NoTemplate: a script-only .vue file
// (no <template>) should pass through unchanged.
func TestVuetifyAutoImport_NoTemplate(t *testing.T) {
	src := `
<script setup lang="ts">
export const x = 1;
</script>
`
	got, n := VuetifyAutoImport(src)
	if n != 0 || got != src {
		t.Errorf("expected no-op on script-only SFC")
	}
}

// TestVuetifyAutoImport_NoScriptSetup: Options-API style SFCs
// (`<script>` without `setup`) shouldn't get auto-imports —
// they'd need the components registered in the `components:`
// block which we can't reliably edit.
func TestVuetifyAutoImport_NoScriptSetup(t *testing.T) {
	src := `
<template>
  <v-btn>Click</v-btn>
</template>

<script>
export default { name: "Foo" };
</script>
`
	got, n := VuetifyAutoImport(src)
	if n != 0 {
		t.Errorf("expected no-op on non-script-setup SFC; injected %d", n)
	}
	if got != src {
		t.Errorf("source mutated despite no-op")
	}
}

// TestVuetifyAutoImport_OrderIsDeterministic: the same SFC
// should produce identical output across runs. Component names
// are alphabetically sorted so the import line is stable +
// bundle hashes don't churn.
func TestVuetifyAutoImport_OrderIsDeterministic(t *testing.T) {
	src := `
<template>
  <v-row>
    <v-col><v-text-field /></v-col>
    <v-col><v-btn /></v-col>
  </v-row>
</template>

<script setup>
</script>
`
	got1, _ := VuetifyAutoImport(src)
	got2, _ := VuetifyAutoImport(src)
	if got1 != got2 {
		t.Errorf("auto-import not deterministic:\nrun1: %s\nrun2: %s", got1, got2)
	}
	// Should be alphabetical: VBtn, VCol, VRow, VTextField
	expected := "VBtn, VCol, VRow, VTextField"
	if !strings.Contains(got1, expected) {
		t.Errorf("expected alphabetical order %q, got:\n%s", expected, got1)
	}
}

// TestVuetifyAutoImport_DedupSameTag: `<v-btn>` used 5 times
// should produce ONE import, not five.
func TestVuetifyAutoImport_DedupSameTag(t *testing.T) {
	src := `
<template>
  <v-btn>One</v-btn>
  <v-btn>Two</v-btn>
  <v-btn>Three</v-btn>
</template>

<script setup>
</script>
`
	got, n := VuetifyAutoImport(src)
	if n != 1 {
		t.Errorf("expected 1 unique import; got %d", n)
	}
	if strings.Count(got, "VBtn") != 1 {
		t.Errorf("VBtn should appear once; got:\n%s", got)
	}
}

// TestKebabToPascal_NestedNames: AppBarTitle from app-bar-title
// — exercises multi-segment kebab conversion.
func TestKebabToPascal_NestedNames(t *testing.T) {
	cases := []struct{ in, want string }{
		{"v-btn", "VBtn"},
		{"v-text-field", "VTextField"},
		{"v-app-bar-title", "VAppBarTitle"},
		{"v-list-item-action", "VListItemAction"},
		{"v-data-table-server", "VDataTableServer"},
		{"v-", ""},
		{"v", ""},
		{"button", ""},
	}
	for _, c := range cases {
		got := kebabToPascal(c.in)
		if got != c.want {
			t.Errorf("kebabToPascal(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
