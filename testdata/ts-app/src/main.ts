import "./style.css";
import { greet } from "./util";

const name: string = "viteless";
document.getElementById("app")!.textContent = greet(name);
