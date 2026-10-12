// worker.js injects this file, then calls claudeFetch from a second script.
globalThis.claudeFetch = async function claudeFetch(url) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 45000);
  try {
