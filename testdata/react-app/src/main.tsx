import { createRoot } from "react-dom/client";

function App() {
  return <h1 className="title">hello from react</h1>;
}

createRoot(document.getElementById("root")!).render(<App />);
