// viteless JS-plugin sidecar. Started by jsplugin.go as:
//   node --input-type=module -e <this> <bundled-config.mjs>
// It imports the (esbuild-bundled) vite/viteless config, exposes the plugins'
// universal hooks, and answers hook calls over newline-delimited JSON on
// stdin/stdout. First line written is the {ready} handshake.
import { pathToFileURL } from "node:url";
import * as readline from "node:readline";

const HOOKS = ["resolveId", "load", "transform", "transformIndexHtml"];

function hookFn(p, h) {
  let f = p && p[h];
  // Vite/Rollup allow an object hook form: { handler, order }.
  if (f && typeof f === "object" && typeof f.handler === "function") f = f.handler;
  return typeof f === "function" ? f : null;
}

// Minimal Rollup/Vite plugin context. Enough for simple plugins; richer
// methods are stubs (a plugin needing this.resolve/emitFile gets a no-op).
function pluginContext() {
  return {
    resolve: async () => null,
    emitFile: () => "",
    getModuleInfo: () => null,
    addWatchFile: () => {},
    warn: () => {},
    error: (m) => { throw m instanceof Error ? m : new Error(String(m)); },
    meta: { framework: "viteless", rollupVersion: "4" },
  };
}

function normalize(hook, r) {
  if (r == null) return null;
  if (hook === "transform" || hook === "load") {
    return typeof r === "string" ? r : (r.code != null ? r.code : null);
  }
  if (hook === "resolveId") {
    return typeof r === "string" ? r : (r.id != null ? r.id : null);
  }
  if (hook === "transformIndexHtml") {
    return typeof r === "string" ? r : (r.html != null ? r.html : null);
  }
  return null;
}

(async () => {
  const cfgPath = process.argv[1];
  const m = await import(pathToFileURL(cfgPath).href);
  let cfg = m && m.default !== undefined ? m.default : m;
  if (typeof cfg === "function") {
    cfg = await cfg({ command: process.env.VL_CMD, mode: process.env.VL_MODE });
  }
  const plugins = (cfg.plugins || []).flat(Infinity).filter(Boolean);
  const meta = plugins.map((p) => ({
    name: p.name || "",
    hooks: HOOKS.filter((h) => hookFn(p, h)),
  }));
  const cfgJSON = JSON.parse(JSON.stringify(cfg, (k, v) => (typeof v === "function" ? undefined : v)));
  process.stdout.write(JSON.stringify({ type: "ready", config: cfgJSON, plugins: meta }) + "\n");

  const rl = readline.createInterface({ input: process.stdin });
  rl.on("line", async (line) => {
    line = line.trim();
    if (!line) return;
    let req;
    try { req = JSON.parse(line); } catch { return; }
    try {
      const p = plugins[req.plugin];
      const f = hookFn(p, req.hook);
      if (!f) { process.stdout.write(JSON.stringify({ id: req.id, value: null }) + "\n"); return; }
      const r = await f.apply(pluginContext(), req.args);
      process.stdout.write(JSON.stringify({ id: req.id, value: normalize(req.hook, r) }) + "\n");
    } catch (e) {
      process.stdout.write(JSON.stringify({ id: req.id, error: String((e && e.stack) || e) }) + "\n");
    }
  });
})().catch((e) => { process.stderr.write(String((e && e.stack) || e)); process.exit(1); });
