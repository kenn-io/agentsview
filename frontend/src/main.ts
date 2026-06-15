import { mount } from "svelte";
import App from "./App.svelte";
import "./app.css";
import { installPerfFetchInstrumentation } from "./lib/stores/perf.svelte.js";

const target = document.getElementById("app");

if (!target) {
  throw new Error("Root element 'app' not found. Cannot mount application.");
}

installPerfFetchInstrumentation();

mount(App, { target });
