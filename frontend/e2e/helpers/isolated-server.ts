import { test as base, expect } from "@playwright/test";
import { execFile, spawn, type ChildProcess } from "node:child_process";
import { once } from "node:events";
import { mkdtemp, rm } from "node:fs/promises";
import { createServer, type AddressInfo } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";

const root = fileURLToPath(new URL("../../../", import.meta.url));
const execute = promisify(execFile);

async function stop(child: ChildProcess): Promise<void> {
  if (!child.pid || child.exitCode !== null || child.signalCode !== null) return;
  const exited = once(child, "exit");
  const timeout = setTimeout(() => {
    try {
      process.kill(-child.pid!, "SIGKILL");
    } catch {
      // The process group may already have exited.
    }
  }, 5_000);
  child.kill("SIGTERM");
  try {
    await exited;
  } finally {
    clearTimeout(timeout);
  }
}

// Each execution, including retries and browser projects, owns its resolver cache
// and mutable fixture archive. Other tests keep using the shared suite server.
export const test = base.extend({
  baseURL: [
    async ({ playwright }, use) => {
      const scratch = await mkdtemp(join(tmpdir(), "agentsview-e2e-"));
      let child: ChildProcess | undefined;
      const probe = await playwright.request.newContext();
      try {
        const binary = async (name: string, supplied: string | undefined) => {
          if (supplied) return supplied;
          const target = join(scratch, name);
          await execute(
            "go",
            ["build", "-tags", "fts5,kit_posthog_disabled", "-o", target, `./cmd/${name}`],
            { cwd: root },
          );
          return target;
        };
        const fixture = await binary("testfixture", process.env.E2E_PREBUILT_FIXTURE);
        const server = await binary("agentsview", process.env.E2E_PREBUILT_SERVER);
        const listener = createServer();
        await new Promise<void>((resolve, reject) => {
          listener.once("error", reject);
          listener.listen(0, "127.0.0.1", resolve);
        });
        const port = (listener.address() as AddressInfo).port;
        await new Promise<void>((resolve, reject) =>
          listener.close((error) => (error ? reject(error) : resolve())),
        );
        const address = `http://127.0.0.1:${port}`;
        child = spawn("bash", [join(root, "scripts/e2e-server.sh")], {
          cwd: root,
          detached: true,
          env: {
            ...process.env,
            AGENTSVIEW_E2E_PORT: String(port),
            E2E_PREBUILT_FIXTURE: fixture,
            E2E_PREBUILT_SERVER: server,
          },
          stdio: ["ignore", "pipe", "pipe"],
        });
        let output = "";
        child.stdout!.on("data", (chunk) => {
          output += chunk;
        });
        child.stderr!.on("data", (chunk) => {
          output += chunk;
        });
        await expect
          .poll(
            async () => {
              if (child!.exitCode !== null || child!.signalCode !== null) throw new Error(output);
              try {
                return (await probe.get(address, { timeout: 1_000 })).ok();
              } catch {
                return false;
              }
            },
            { timeout: 30_000 },
          )
          .toBe(true);
        await use(address);
      } finally {
        if (child) await stop(child);
        await probe.dispose();
        await rm(scratch, { recursive: true, force: true });
      }
    },
    { scope: "test", timeout: 120_000 },
  ],
});

export { expect };
