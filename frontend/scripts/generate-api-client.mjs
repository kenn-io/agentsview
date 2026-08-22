import { spawnSync } from "node:child_process";
import { existsSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, relative, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const frontendDir = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const repoRoot = resolve(frontendDir, "..");

function suppressExpectedAbortLogging() {
  const requestPath = join(frontendDir, "src/lib/api/generated/core/request.ts");
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
    throw new Error("generated request body handler no longer matches the abort-log patch");
  }
  writeFileSync(requestPath, source.replace(generatedCatch, cancellationAwareCatch));
}

function preserveExplicitAuthorizationHeaders() {
  const requestPath = join(frontendDir, "src/lib/api/generated/core/request.ts");
  const source = readFileSync(requestPath, "utf8");
  const generatedAuth = `  if (isStringWithValue(token)) {
    headers['Authorization'] = \`Bearer \${token}\`;
  }

  if (isStringWithValue(username) && isStringWithValue(password)) {
    const credentials = base64(\`\${username}:\${password}\`);
    headers['Authorization'] = \`Basic \${credentials}\`;
  }
`;
  const explicitHeaderFirst = `  if (isStringWithValue(token) && !isStringWithValue(headers['Authorization'])) {
    headers['Authorization'] = \`Bearer \${token}\`;
  }

  if (
    isStringWithValue(username) &&
    isStringWithValue(password) &&
    !isStringWithValue(headers['Authorization'])
  ) {
    const credentials = base64(\`\${username}:\${password}\`);
    headers['Authorization'] = \`Basic \${credentials}\`;
  }
`;
  if (!source.includes(generatedAuth)) {
    throw new Error("generated request headers no longer match the authorization patch");
  }
  writeFileSync(requestPath, source.replace(generatedAuth, explicitHeaderFirst));
}

function generatedTrailingNewlineCounts(dir, base = dir, counts = new Map()) {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) {
      generatedTrailingNewlineCounts(path, base, counts);
      continue;
    }
    if (!entry.name.endsWith(".ts")) continue;
    const source = readFileSync(path, "utf8");
    counts.set(relative(base, path), source.match(/\n+$/)?.[0].length ?? 0);
  }
  return counts;
}

function generatedSources(dir, base = dir, sources = new Map()) {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) {
      generatedSources(path, base, sources);
      continue;
    }
    if (!entry.name.endsWith(".ts")) continue;
    sources.set(relative(base, path), readFileSync(path, "utf8"));
  }
  return sources;
}

function normalizeGeneratedTrailingNewlines(dir, preferred, base = dir) {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) {
      normalizeGeneratedTrailingNewlines(path, preferred, base);
      continue;
    }
    if (!entry.name.endsWith(".ts")) continue;
    const source = readFileSync(path, "utf8");
    const newlineCount = preferred.get(relative(base, path)) ?? 1;
    const normalized = source.replace(/\n*$/, "\n".repeat(newlineCount));
    if (normalized !== source) writeFileSync(path, normalized);
  }
}

// openapi-typescript-codegen 0.31 treats OpenAPI 3.1 nullable array type
// unions as untyped arrays. Normalize affected response arrays to the
// equivalent nullable form it understands before generation.
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

const generatedDir = join(frontendDir, "src/lib/api/generated");
const checkIfGoAvailable = process.argv.includes("--check-if-go-available");
const verifyGenerated = process.argv.includes("--check") || checkIfGoAvailable;
if (checkIfGoAvailable) {
  const goVersion = spawnSync("go", ["version"], { stdio: "ignore" });
  if (goVersion.error?.code === "ENOENT") {
    console.warn("Skipping generated API client check because Go is not available.");
    process.exit(0);
  }
}
const previousGeneratedSources = verifyGenerated ? generatedSources(generatedDir) : null;
const generatedTrailingNewlines = generatedTrailingNewlineCounts(generatedDir);
const tempDir = mkdtempSync(join(tmpdir(), "agentsview-openapi-"));
try {
  const pricingSnapshot = join(repoRoot, "internal/pricing/snapshot/litellm_snapshot.json.gz");
  if (!existsSync(pricingSnapshot)) {
    run("go", ["run", "./internal/pricing/cmd/litellm-snapshot"], {
      cwd: repoRoot,
    });
  }
  const specPath = join(tempDir, "openapi.json");
  run("go", ["run", "./internal/pricing/cmd/litellm-snapshot", "-restore"], {
    cwd: repoRoot,
  });
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
    ["SessionProviderResponse", "dirs"],
    ["SettingsResponse", "disabled_agents"],
    ["SettingsResponse", "session_providers"],
    ["SyncWatchBatch", "paths"],
    ["SyncWatchBatch", "reconcile_roots"],
    ["SyncWatchBatch", "renames"],
    ["SyncWatchRecoveryScope", "available_roots"],
    ["SyncWatchRecoveryScope", "deferred_roots"],
  ]) {
    normalizeNullableArray(schemas?.[model]?.properties?.[property], `${model}.${property}`);
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
    run(process.env.ComSpec ?? "cmd.exe", ["/d", "/s", "/c", "npx.cmd", ...openapiArgs], {
      cwd: frontendDir,
    });
  } else {
    run("npx", openapiArgs, { cwd: frontendDir });
  }
  suppressExpectedAbortLogging();
  preserveExplicitAuthorizationHeaders();
  normalizeGeneratedTrailingNewlines(generatedDir, generatedTrailingNewlines);
} finally {
  rmSync(tempDir, { recursive: true, force: true });
}

if (previousGeneratedSources) {
  const currentGeneratedSources = generatedSources(generatedDir);
  const paths = new Set([
    ...previousGeneratedSources.keys(),
    ...currentGeneratedSources.keys(),
  ]);
  const changed = [...paths].filter(
    (path) => previousGeneratedSources.get(path) !== currentGeneratedSources.get(path),
  );
  if (changed.length > 0) {
    throw new Error(
      `generated API client was stale and has been regenerated:\n${changed.join("\n")}`,
    );
  }
}
