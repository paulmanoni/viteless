import { useState } from "react";

export function Counter({ step }: { step: number }) {
  const [count, setCount] = useState(0);
  return (
    <div className="counter">
      <button onClick={() => setCount((c) => c - step)}>−</button>
      <span className="value">{count}</span>
      <button onClick={() => setCount((c) => c + step)}>+</button>
    </div>
  );
}
