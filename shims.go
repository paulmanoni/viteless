package viteless

// JS shims injected when evaluating a vite.config in QuickJS. They stand in
// for `vite` and a few node builtins so the config module evaluates without
// Node and without pulling in real Vite (which can't run in WASM anyway).

// viteShimJS replaces the `vite` package. defineConfig/mergeConfig are the
// only members a config actually needs at evaluation time; both are
// essentially identity.
const viteShimJS = `
export const defineConfig = (c) => c;
export const mergeConfig = (a, b) => Object.assign({}, a || {}, b || {});
export const loadEnv = () => ({});
export const normalizePath = (p) => p;
export default { defineConfig, mergeConfig, loadEnv, normalizePath };
`

// nodeShimFor returns a minimal JS module for a node builtin (already
// stripped of the "node:" prefix). Only the members configs commonly touch
// during evaluation are implemented; everything else is a harmless no-op.
func nodeShimFor(name string) string {
	switch name {
	case "path":
		return nodePathShim
	case "url":
		return nodeURLShim
	case "os":
		return nodeOSShim
	default: // fs, process, util, module, …
		return nodeEmptyShim
	}
}

const nodePathShim = `
function clean(p){ return String(p).replace(/\/+/g, '/'); }
export function join(){ return clean(Array.prototype.filter.call(arguments, Boolean).join('/')); }
export function resolve(){ return clean(Array.prototype.filter.call(arguments, Boolean).join('/')); }
export function dirname(p){ p = String(p); var i = p.lastIndexOf('/'); return i < 0 ? '.' : (i === 0 ? '/' : p.slice(0, i)); }
export function basename(p, ext){ p = String(p); var b = p.slice(p.lastIndexOf('/') + 1); if (ext && b.slice(-ext.length) === ext) b = b.slice(0, -ext.length); return b; }
export function extname(p){ p = String(p); var b = p.slice(p.lastIndexOf('/') + 1); var i = b.lastIndexOf('.'); return i > 0 ? b.slice(i) : ''; }
export const sep = '/';
export const posix = { join, resolve, dirname, basename, extname, sep: '/' };
export default { join, resolve, dirname, basename, extname, sep: '/', posix };
`

const nodeURLShim = `
export function fileURLToPath(u){ u = String(u); u = u.replace(/^file:\/\//, ''); try { return decodeURIComponent(u); } catch (e) { return u; } }
export function pathToFileURL(p){ p = String(p); return { href: 'file://' + p, pathname: p, toString: function(){ return 'file://' + p; } }; }
export class URL {
  constructor(input, base){
    input = String(input);
    if (base !== undefined && base !== null) {
      var b = String(base).replace(/^file:\/\//, '');
      var dir = b.slice(0, b.lastIndexOf('/'));
      var rel = input.replace(/^\.\//, '');
      this.pathname = (dir + '/' + rel).replace(/\/+/g, '/');
    } else {
      this.pathname = input.replace(/^file:\/\//, '');
    }
    this.href = 'file://' + this.pathname;
  }
  toString(){ return this.href; }
}
export default { fileURLToPath, pathToFileURL, URL };
`

const nodeOSShim = `
export function platform(){ return 'linux'; }
export function homedir(){ return '/'; }
export function tmpdir(){ return '/tmp'; }
export const EOL = '\n';
export default { platform, homedir, tmpdir, EOL };
`

const nodeEmptyShim = `
export function noop(){}
export const __viteless_empty = true;
export default {};
`
