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

// openapi-typescript-codegen 0.31 treats OpenAPI 3.1 nullable array type
// unions as untyped arrays. Normalize the pricing arrays added in schema v4
// to the equivalent nullable form it understands before generation.
function normalizeNullableArray(schema, label) {
  if (!schema || !Array.isArray(schema.type) || !schema.type.includes("null")) {
    throw new Error(`missing nullable OpenAPI array schema for ${label}`);
  }
  const concreteTypes = schema.type.filter((item) => item !== "null");
  if (concreteTypes.length !== 1) {
    throw new Error(
      `cannot normalize nullable OpenAPI type for ${label}: ${JSON.stringify(schema.type)}`,
    );
  }
  schema.type = concreteTypes[0];
  schema.nullable = true;
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
  const parsedSpec = JSON.parse(spec);
  const schemas = parsedSpec.components?.schemas;
  for (const [model, property] of [
    ["ExportEffectiveModelRate", "bands"],
    ["ExportPricingApplication", "bands"],
    ["ExportModelPricingProvenance", "resolutions"],
  ]) {
    normalizeNullableArray(
      schemas?.[model]?.properties?.[property],
      `${model}.${property}`,
    );
  }
  writeFileSync(specPath, JSON.stringify(parsedSpec));
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
} finally {
  rmSync(tempDir, { recursive: true, force: true });
}
