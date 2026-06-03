package resolver

import "testing"

func TestEsmRangePackage(t *testing.T) {
	// Unresolved semver ranges → (package, true).
	ranges := map[string]string{
		"https://esm.sh/graphql@%5E15.0.0%20%7C%7C%20%5E16.0.0?target=es2022": "graphql",
		"https://esm.sh/graphql@^15.0.0 || ^16.0.0?target=es2022":             "graphql",
		"https://esm.sh/@wry/caches@%5E1.0.0?target=es2022":                   "@wry/caches",
		"https://esm.sh/graphql-tag@^2.12.6?target=es2022":                    "graphql-tag",
		"https://esm.sh/foo@~1.2.0":                                           "foo",
		"https://esm.sh/bar@*":                                                "bar",
	}
	for u, wantPkg := range ranges {
		gotPkg, ok := esmRangePackage(u)
		if !ok || gotPkg != wantPkg {
			t.Errorf("esmRangePackage(%q) = (%q,%v), want (%q,true)", u, gotPkg, ok, wantPkg)
		}
	}
	// Concrete versions → not a range.
	concrete := []string{
		"https://esm.sh/graphql@16.13.0?external=vue,react,react-dom",
		"https://esm.sh/vue@3.5.34/es2022/vue.mjs",
		"https://esm.sh/@vue/runtime-core@3.5.34/es2022/x.mjs",
		"/src/main.ts",
	}
	for _, u := range concrete {
		if _, ok := esmRangePackage(u); ok {
			t.Errorf("esmRangePackage(%q) = range, want concrete/non-match", u)
		}
	}
}
