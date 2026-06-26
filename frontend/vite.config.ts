import { execSync } from "node:child_process";
import { defineConfig } from "vite-plus";
import { svelte } from "@sveltejs/vite-plugin-svelte";
import { paraglideVitePlugin } from "@inlang/paraglide-js";

function gitCommit(): string {
  try {
    return execSync("git rev-parse --short HEAD", {
      encoding: "utf-8",
    }).trim();
  } catch {
    return "unknown";
  }
}

const apiTarget = process.env.VITE_API_TARGET ?? "http://127.0.0.1:8080";
const apiTargetOrigin = new URL(apiTarget).origin;

function isIPv4LoopbackLiteral(hostname: string): boolean {
  const parts = hostname.split(".");
  if (parts.length !== 4 || parts[0] !== "127") return false;
  return parts.every((part) => {
    if (!/^\d+$/.test(part)) return false;
    const value = Number(part);
    return value >= 0 && value <= 255;
  });
}

function isLoopbackHostname(hostname: string): boolean {
  const lower = hostname.toLowerCase();
  const unbracketed = lower.startsWith("[") && lower.endsWith("]")
    ? lower.slice(1, -1)
    : lower;
  return lower === "localhost" ||
    isIPv4LoopbackLiteral(unbracketed) ||
    unbracketed === "::1";
}

function requestOriginMatchesLoopbackDevServer(
  origin: string | undefined,
  host: string | undefined,
): boolean {
  if (!origin || !host) return false;
  try {
    const originURL = new URL(origin);
    const hostURL = new URL(`${originURL.protocol}//${host}`);
    return originURL.host === hostURL.host &&
      isLoopbackHostname(originURL.hostname);
  } catch {
    return false;
  }
}

function isViteDevOrigin(
  origin: string | undefined,
  host: string | undefined,
): boolean {
  if (!origin || !host) return false;
  try {
    const u = new URL(origin);
    return u.protocol === "http:" && u.host === host;
  } catch {
    return false;
  }
}

export default defineConfig({
  fmt: {},
  lint: {
    jsPlugins: [
      { name: "vite-plus", specifier: "vite-plus/oxlint-plugin" },
    ],
    rules: {
      "vite-plus/prefer-vite-plus-imports": "error",
    },
    options: {
      typeAware: true,
      typeCheck: true,
    },
  },
  base: "/",
  plugins: [
    svelte(),
    paraglideVitePlugin({
      project: "./project.inlang",
      outdir: "./src/lib/paraglide",
      emitTsDeclarations: true,
      strategy: ["localStorage", "preferredLanguage", "baseLocale"],
      localStorageKey: "agentsview-locale",
    }),
  ],
  define: {
    "import.meta.env.VITE_BUILD_COMMIT": JSON.stringify(
      gitCommit(),
    ),
  },
  resolve: {
    conditions: ["browser"],
  },
  server: {
    proxy: {
      "/api": {
        target: apiTarget,
        changeOrigin: true,
        configure(proxy) {
          proxy.on("proxyReq", (proxyReq, req) => {
            const origin = req.headers.origin;
            if (
              requestOriginMatchesLoopbackDevServer(
                typeof origin === "string" ? origin : undefined,
                req.headers.host,
              )
            ) {
              proxyReq.setHeader("Origin", apiTargetOrigin);
            }
          });
        },
      },
    },
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
  },
  test: {
    environment: "jsdom",
    exclude: ["e2e/**", "node_modules/**"],
    setupFiles: ["./src/vitest-setup.ts"],
    server: {
      deps: {
        inline: ["svelte"],
      },
    },
  },
});
