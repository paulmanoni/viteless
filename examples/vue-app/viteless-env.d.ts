// Ambient declarations so the editor resolves a viteless project's imports
// with nothing installed. viteless reads viteless.config.ts itself; these
// types just keep the TypeScript language server quiet.

declare module "viteless" {
  /** Identity helper — viteless reads the returned config statically. */
  export function defineConfig<T>(config: T): T
}

declare module "@vitejs/plugin-vue" {
  // viteless recognizes this plugin by name and compiles .vue natively; the
  // plugin's JS is never executed. The type is intentionally permissive.
  const plugin: (...args: any[]) => any
  export default plugin
}

// Allow importing single-file components.
declare module "*.vue" {
  import type { DefineComponent } from "vue"
  const component: DefineComponent<{}, {}, any>
  export default component
}
