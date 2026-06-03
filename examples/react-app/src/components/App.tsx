import { Counter } from "./Counter";

export function App() {
  return (
    <main className="app">
      <h1>
        👋 Hello from <span className="brand">viteless</span> + React
      </h1>
      <p className="hint">
        Edit <code>src/components/App.tsx</code> or <code>src/components/Counter.tsx</code> and
        save — the page reloads with your change (React Fast Refresh is a viteless follow-up,
        so component state resets on edit for now).
      </p>
      <Counter step={1} />
    </main>
  );
}
