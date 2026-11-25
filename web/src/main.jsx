// Not in StrictMode: its doubled effects would ask the brokers everything twice
import { createRoot } from "react-dom/client";
import "./styles.css";
import "./desk.css";
import "./metals.css";
import App from "./App.jsx";

createRoot(document.getElementById("root")).render(<App />);
