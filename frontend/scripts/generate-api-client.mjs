import { spawnSync } from "node:child_process";
import {
  mkdtempSync,
  readFileSync,
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

import { normalizeChangedGeneratedFiles } from "./generated-file-normalizer.mjs";

const frontendDir = resolve(
  dirname(fileURLToPath(import.meta.url)),
  "..",
);
const repoRoot = resolve(frontendDir, "..");

function suppressExpectedAbortLogging() {
  const requestPath = join(
    frontendDir,
    "src/lib/api/generated/core/request.ts",
  );
  const source = readFileSync(requestPath, "utf8");
  const generatedCatch = `    } catch (error) {
      console.error(error);
    }
`;
  const cancellationAwareCatch = `    } catch (error) {
      if (!(error instanceof DOMException && error.name === 'AbortError')) {
        console.error(error);
      }
    }
`;
  if (!source.includes(generatedCatch)) {
    throw new Error(
      "generated request body handler no longer matches the abort-log patch",
    );
  }
  writeFileSync(
    requestPath,
    source.replace(generatedCatch, cancellationAwareCatch),
  );
}

function preserveBinaryResponseBodies() {
  const requestPath = join(
    frontendDir,
    "src/lib/api/generated/core/request.ts",
  );
  const source = readFileSync(requestPath, "utf8");
  const generatedDecoder = `        const jsonTypes = ['application/json', 'application/problem+json']
        const isJSON = jsonTypes.some(type => contentType.toLowerCase().startsWith(type));
        if (isJSON) {
          return await response.json();
        } else {
          return await response.text();
        }
`;
  const binaryAwareDecoder = `        const normalizedContentType = contentType.toLowerCase();
        const jsonTypes = ['application/json', 'application/problem+json'];
        const binaryTypes = ['application/octet-stream', 'application/zstd'];
        const isJSON = jsonTypes.some(type => normalizedContentType.startsWith(type));
        const isBinary = binaryTypes.some(type => normalizedContentType.startsWith(type));
        if (isJSON) {
          return await response.json();
        } else if (isBinary) {
          return await response.blob();
        } else {
          return await response.text();
        }
`;
  if (!source.includes(generatedDecoder)) {
    throw new Error(
      "generated request body handler no longer matches the binary-body patch",
    );
  }
  writeFileSync(
    requestPath,
    source.replace(generatedDecoder, binaryAwareDecoder),
  );
}

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
  const spec = run("go", ["run", "./cmd/agentsview", "openapi"], {
    cwd: repoRoot,
    capture: true,
  });
  writeFileSync(specPath, spec);
  const openapiArgs = [
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
  ];
  if (process.platform === "win32") {
    run(
      process.env.ComSpec ?? "cmd.exe",
      ["/d", "/s", "/c", "npx.cmd", ...openapiArgs],
      { cwd: frontendDir },
    );
  } else {
    run("npx", openapiArgs, { cwd: frontendDir });
  }
  suppressExpectedAbortLogging();
  preserveBinaryResponseBodies();
  normalizeChangedGeneratedFiles(repoRoot);
} finally {
  rmSync(tempDir, { recursive: true, force: true });
}
