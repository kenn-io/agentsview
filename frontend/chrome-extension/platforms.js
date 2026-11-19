// Each key is a platform's agent type, the name the server sends. requests/
// holds one allowlist per key, copied from internal/importer/browser_requests.
// headers, when set, adds request headers; worker.js passes them to
// fetchFunction after the URL.
const uuid = "[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}";

export const platforms = {
  "claude-ai": {
    origin: "https://claude.ai",
    newTabURL: "https://claude.ai/new",
    scripts: ["page_fetch.js"],
    fetchFunction: "pageFetch",
    // Patterns for each {placeholder} in the allowlist.
    params: { organization: uuid, conversation: uuid, offset: "[0-9]+" },
  },
  chatgpt: {
    origin: "https://chatgpt.com",
    newTabURL: "https://chatgpt.com/",
    scripts: ["page_fetch.js", "chatgpt_fetch.js"],
    fetchFunction: "chatgptFetch",
    params: { conversation: uuid, offset: "[0-9]+", archived: "(?:false|true)" },
  },
  "gemini-apps": {
    origin: "https://gemini.google.com",
    newTabURL: "https://gemini.google.com/app",
    scripts: ["page_fetch.js", "gemini_fetch.js"],
    fetchFunction: "geminiFetch",
    // Values arrive query-escaped, so a cursor cannot add a parameter.
    params: { chat: "c_[0-9A-Za-z_-]+", cursor: "[0-9A-Za-z%._~-]*" },
  },
  "grok-web": {
    origin: "https://grok.com",
    newTabURL: "https://grok.com/",
    scripts: ["page_fetch.js"],
    fetchFunction: "pageFetch",
    // grok.com reads its REST API with the page's cookies; the allowed
    // shapes are plain GETs that ask for JSON.
    headers: { Accept: "application/json" },
    params: { conversation: uuid, pageToken: uuid },
  },
};
