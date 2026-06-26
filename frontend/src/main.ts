import { mount } from "svelte";
import App from "./App.svelte";
import "./app.css";
import { initI18n } from "./lib/i18n/index.js";
import { installPerfFetchInstrumentation } from "./lib/stores/perf.svelte.js";

const target = document.getElementById("app");

if (!target) {
  throw new Error("Root element 'app' not found. Cannot mount application.");
}

installPerfFetchInstrumentation();
initI18n();

mount(App, { target });
