import { spawnSync } from "node:child_process";
import {
  mkdtempSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import {
  dirname,
  join,
  resolve,
} from "node:path";
import { fileURLToPath } from "node:url";

const frontendDir = resolve(
  dirname(fileURLToPath(import.meta.url)),
  "..",
);
const repoRoot = resolve(frontendDir, "..");

function run(cmd, args, options = {}) {
  const result = spawnSync(cmd, args, {
    cwd: options.cwd,
    encoding: "utf8",
    stdio: options.capture ? ["ignore", "pipe", "pipe"] : "inherit",
  });
  if (result.error) throw result.error;
  if (result.status !== 0) {
    throw new Error(`${cmd} ${args.join(" ")} exited ${result.status}`);
  }
  return result.stdout ?? "";
}

const tempDir = mkdtempSync(join(tmpdir(), "agentsview-openapi-"));
try {
  const specPath = join(tempDir, "openapi.json");
  const specPathForShell = specPath.replaceAll("\\", "/");
  const spec = run("go", ["run", "./cmd/agentsview", "openapi"], {
    cwd: repoRoot,
    capture: true,
  });
  writeFileSync(specPath, spec);
  if (process.platform === "win32") {
    run(
      "cmd",
      [
        "/d",
        "/s",
        "/c",
        `npx openapi -i ${specPathForShell} -o src/lib/api/generated -c fetch --useOptions --indent 2`,
      ],
      { cwd: frontendDir },
    );
  } else {
    run(
      "npx",
      [
        "openapi",
        "-i",
        specPath,
        "-o",
        "src/lib/api/generated",
        "-c",
        "fetch",
        "--useOptions",
        "--indent",
        "2",
      ],
      { cwd: frontendDir },
    );
  }
} finally {
  rmSync(tempDir, { recursive: true, force: true });
}
