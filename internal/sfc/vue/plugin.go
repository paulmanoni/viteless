package vue

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/evanw/esbuild/pkg/api"

	"github.com/paulmanoni/viteless/internal/bundler"
)

// Plugin wraps an SFCCompiler in an esbuild plugin so the bundler
// picks up .vue files transparently. Register with
// Bundler.AddPlugin AFTER the resolver plugin (resolver handles
// bare imports; this one handles .vue file loads).
//
// Compile errors surface as esbuild Messages, which causes the
// build to fail loudly with the file + line + col + message. We
// do NOT emit any code when the compiler reports errors — better
// to fail than to feed esbuild half-compiled JS that produces a
// second cascade of errors.
func Plugin(c SFCCompiler) (api.Plugin, error) {
	if c == nil {
		return api.Plugin{}, errors.New("vue: Plugin called with nil Compiler")
	}
	return api.Plugin{
		Name: "nexus-vue-sfc",
		Setup: func(build api.PluginBuild) {
			build.OnLoad(api.OnLoadOptions{Filter: `\.vue$`}, func(args api.OnLoadArgs) (api.OnLoadResult, error) {
				source, err := os.ReadFile(args.Path)
				if err != nil {
					return api.OnLoadResult{}, fmt.Errorf("vue: read %s: %w", args.Path, err)
				}
				// Vuetify auto-import: scan the SFC template
				// for `<v-*>` tags and inject the matching
				// imports into <script setup>. No-op when the
				// file isn't a Vuetify-using SFC or already
				// has manual `vuetify/components` import.
				rewritten, _ := bundler.VuetifyAutoImport(string(source))
				// SASS/SCSS preprocessing is intentionally unsupported:
				// inline <style lang="scss"> blocks are passed through to
				// the adapter as-is. Plain CSS and Tailwind are the
				// supported style paths.
				res, err := c.Compile(rewritten, args.Path)
				if err != nil {
					return api.OnLoadResult{
						Errors: []api.Message{{
							Text:     err.Error(),
							Location: &api.Location{File: args.Path},
						}},
					}, nil
				}
				if len(res.Errors) > 0 {
					msgs := make([]api.Message, 0, len(res.Errors))
					for _, ce := range res.Errors {
						msgs = append(msgs, api.Message{
							Text: ce.Message,
							Location: &api.Location{
								File:   args.Path,
								Line:   ce.Line,
								Column: ce.Column,
							},
						})
					}
					return api.OnLoadResult{Errors: msgs}, nil
				}
				contents := res.Code
				// Run import.meta.glob rewriting on the compiled
				// JS so calls inside <script setup> blocks (a
				// common Vue Router auto-discovery pattern) get
				// expanded. esbuild won't re-run OnLoad on a
				// plugin's output, so we have to do this here
				// or globs inside .vue files would ship to the
				// browser as live calls and fail at runtime.
				//
				// baseDir is the .vue file's directory so
				// relative patterns ('./pages/*.vue') resolve
				// against the SFC's neighborhood, matching what
				// import.meta.glob's call site in the SFC
				// expects.
				if rewritten, n, gerr := bundler.RewriteGlobCalls(contents, filepath.Dir(args.Path)); gerr == nil && n > 0 {
					contents = rewritten
				} else if gerr != nil {
					return api.OnLoadResult{
						Errors: []api.Message{{
							Text:     fmt.Sprintf("vue: %v", gerr),
							Location: &api.Location{File: args.Path},
						}},
					}, nil
				}
				// Use the TS loader so esbuild strips the type
				// annotations @vue/compiler-sfc emits when the
				// SFC uses `<script setup lang="ts">`. The
				// compiled template wrapper itself also carries
				// `(_ctx: any, _cache: any) =>` annotations, so
				// even pure-JS SFCs benefit. LoaderJS would fail
				// on those with "Expected ')' but found ':'".
				return api.OnLoadResult{
					Contents: &contents,
					Loader:   api.LoaderTS,
				}, nil
			})
		},
	}, nil
}
