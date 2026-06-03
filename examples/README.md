# viteless examples

Runnable example apps for the `viteless` zero-Node frontend toolchain.

## Two dependency modes (auto-detected)

viteless picks where to source npm packages automatically per project:

- **Zero-install (default).** No `node_modules`? Dependencies (Vue, React) are
  fetched from the esm.sh registry on first run and cached under
  `~/.viteless/cache`. Nothing to install — just run.
- **node_modules.** Ran `npm install`? viteless detects `node_modules` and
  resolves everything from it instead of the CDN — offline, exact installed
  versions. In dev it optimizes each dependency with esbuild (like Vite's
  `optimizeDeps`); in build esbuild resolves them natively.

Either way the runtime is a single Go binary with no Node. `package.json`
versions are honored: in CDN mode a declared range like `"vue": "^3.5"` pins
the fetched version; in node_modules mode the installed copy is used.

## Config: `viteless.config.ts` (and `vite.config.ts`)

viteless reads a `viteless.config.ts` (preferred) or a `vite.config.ts`
(drop-in compat) and applies its static surface — `base`, `plugins`,
`resolve.alias`, `server.proxy`, `build.outDir`. See `vue-app/viteless.config.ts`.

- Import `defineConfig` from `'viteless'` (no `vite` package needed).
- Recognized plugins (`@vitejs/plugin-vue`, `@vitejs/plugin-react`,
  `@tailwindcss/vite`) are handled by viteless's native paths — their JS is
  never run. Unrecognized plugins are reported as unsupported.
- **Node-compatible:** if `node` is on PATH, viteless evaluates the config
  with real Node (full fidelity — real `node:*` builtins and installed
  plugins). With no Node, it evaluates the config in an embedded QuickJS
  engine (zero-Node). Either way you get the same config surface.
- A relative alias (`'@': './src'`) is resolved against the project root, so
  no `node:url`/`import.meta.url` dance is needed. `tsconfig.json` +
  `viteless-env.d.ts` keep the editor's TypeScript happy with nothing installed.

## How viteless picks an engine (highest fidelity available)

1. **Real Vite installed** (`node_modules/.bin/vite` present) → viteless
   **delegates to Vite** for dev/build. 100% compatibility, every plugin works;
   viteless is just the launcher. Force the viteless engine instead with
   `VITELESS_ENGINE=1`.
2. **Node on PATH (no Vite)** → viteless's own engine, with a **Node sidecar**
   that evaluates `vite.config`/`viteless.config` and runs real JS plugins
   (tier-2) at full fidelity, plus native Vue/React/Tailwind (tier-1).
3. **No Node** → fully zero-Node: native tier-1 plugins, config evaluated in an
   embedded QuickJS engine, dependencies from esm.sh. JS-only plugins are
   reported as unsupported.

Same project, same config — viteless uses the best engine your environment
offers and degrades gracefully.

A tiny CLI (`cmd/viteless`) wraps the library so the examples can run without a
host framework:

```
viteless dev   <dir> [-addr host:port] [-proxy origin]
viteless build <dir> [-out dir]
```

## Try it

From the repo root (`~/Documents/personal/viteless`):

```sh
# Vue 3 + state-preserving HMR
go run ./cmd/viteless dev ./examples/vue-app
# → open http://localhost:5173

# React (automatic JSX runtime)
go run ./cmd/viteless dev ./examples/react-app -addr 127.0.0.1:5174
# → open http://localhost:5174
```

Then edit a component and save:

- **vue-app** — change the heading in `src/App.vue` or the button styling in
  `src/components/Counter.vue`. The counter value **survives** the update
  (state-preserving HMR via Vue's HMR runtime).
- **react-app** — change `src/components/App.tsx` or `Counter.tsx`. The page
  reloads with your change. (React Fast Refresh is a viteless follow-up, so
  component state resets on edit for now.)

The first request after boot fetches the framework from esm.sh and pre-bundles
it (a one-time, cached step), so the very first paint may take a moment;
subsequent loads and edits are instant.

### Run from node_modules instead of the CDN

```sh
cd examples/vue-app
npm install            # installs vue into node_modules
cd ../..
go run ./cmd/viteless dev ./examples/vue-app   # auto-detects node_modules
```

No flag needed — viteless sees `node_modules` and switches to it. Delete
`node_modules` to go back to the zero-install CDN path.

## Production build

```sh
go run ./cmd/viteless build ./examples/vue-app
# → examples/vue-app/dist/  (index.html + hashed JS/CSS/chunks)
```

The output in `dist/` is a static SPA: an `index.html` referencing the built
entry plus its extracted CSS. In a real app this is what a Go binary embeds via
`//go:embed` and serves — one binary, no Node at runtime.

> First build needs network access to esm.sh. `dist/` and the generated
> `nexus.lock` are git-ignored.
