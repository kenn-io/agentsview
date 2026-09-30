import { spawnSync } from "node:child_process";
import { existsSync, readFileSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join, relative, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { tmpdir } from "node:os";
import { generate } from "orval";

const frontendDir = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const repoRoot = resolve(frontendDir, "..");
const generatedDir = join(frontendDir, "src/lib/api/generated");

function generatedSources(dir, base = dir, sources = new Map()) {
  if (!existsSync(dir)) return sources;
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

function operationName(operation, route) {
  if (!operation.operationId) throw new Error("Every API operation must have an operationId");
  let name = operation.operationId;
  for (const match of route.matchAll(/\{([^}]+)\}/g)) {
    const parameter = match[1]
      .replace(/([a-z0-9])([A-Z])/g, "$1-$2")
      .replaceAll("_", "-")
      .toLowerCase();
    for (const candidate of [parameter, parameter.replaceAll("-", "")]) {
      const next = name.replace(`-${candidate}`, `-by-${parameter}`);
      if (next !== name) {
        name = next;
        break;
      }
    }
  }
  return name.replace(/-([a-z0-9])/g, (_, character) => character.toUpperCase());
}

function writeIndex() {
  const exports = ['export * from "./models/index.ts";'];
  for (const entry of readdirSync(generatedDir, { withFileTypes: true })) {
    if (!entry.isDirectory() || entry.name === "models") continue;
    const serviceName = `${entry.name
      .split("-")
      .map((part) => part[0].toUpperCase() + part.slice(1))
      .join("")}Service`;
    exports.push(`export * as ${serviceName} from "./${entry.name}/${entry.name}.ts";`);
  }
  writeFileSync(join(generatedDir, "index.ts"), `${exports.sort().join("\n")}\n`);
}

function orderLedgerPathsLast(spec) {
  const lines = spec.split("\n");
  const pathsIndex = lines.findIndex((line) => line === "paths:");
  if (pathsIndex < 0) throw new Error("OpenAPI document has no paths section");

  const pathsEnd = lines.findIndex(
    (line, index) => index > pathsIndex && /^[A-Za-z][A-Za-z0-9_-]*:\s*$/.test(line),
  );
  const end = pathsEnd < 0 ? lines.length : pathsEnd;
  const prefix = [];
  const blocks = [];
  let current = null;
  for (const line of lines.slice(pathsIndex + 1, end)) {
    const path = line.match(/^  (\/\S+):\s*$/)?.[1];
    if (path) {
      if (current) blocks.push(current);
      current = { path, lines: [line] };
    } else if (current) {
      current.lines.push(line);
    } else {
      prefix.push(line);
    }
  }
  if (current) blocks.push(current);

  const ledgerPaths = new Set([
    "/api/v1/ledger/events",
    "/api/v1/ledger/status",
    "/api/v1/ledger/verify",
  ]);
  const present = blocks.filter(({ path }) => ledgerPaths.has(path));
  if (present.length === 0) return spec;
  if (present.length !== ledgerPaths.size) {
    throw new Error("OpenAPI document has an incomplete ledger path set");
  }

  const ordered = [
    ...blocks.filter(({ path }) => !ledgerPaths.has(path)),
    ...present,
  ];
  return [
    ...lines.slice(0, pathsIndex + 1),
    ...prefix,
    ...ordered.flatMap(({ lines: block }) => block),
    ...lines.slice(end),
  ].join("\n");
}

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
const specPath = join(repoRoot, "openapi.yaml");
const previousSpec =
  verifyGenerated && existsSync(specPath) ? readFileSync(specPath, "utf8") : null;
const pricingSnapshot = join(repoRoot, "internal/pricing/snapshot/litellm_snapshot.json.gz");
if (!existsSync(pricingSnapshot)) {
  run("go", ["run", "./internal/pricing/cmd/litellm-snapshot"], { cwd: repoRoot });
}
run("go", ["run", "./internal/pricing/cmd/litellm-snapshot", "-restore"], {
  cwd: repoRoot,
});
const spec = run("go", ["run", "./cmd/agentsview", "openapi", "--yaml"], {
  cwd: repoRoot,
  capture: true,
});
writeFileSync(specPath, spec);

const goClientPath = join(repoRoot, "internal/apiclient/client.gen.go");
const previousGoClient =
  verifyGenerated && existsSync(goClientPath) ? readFileSync(goClientPath, "utf8") : null;
const goClientSpecPath = join(tmpdir(), `agentsview-api-client-${process.pid}.yaml`);
writeFileSync(goClientSpecPath, orderLedgerPathsLast(spec));
try {
  run(
    "go",
    [
      "run",
      "github.com/doordash-oss/oapi-codegen-dd/v3/cmd/oapi-codegen@v3.75.15",
      "--config",
      "internal/apiclient/generate.yaml",
      goClientSpecPath,
    ],
    { cwd: repoRoot },
  );
} finally {
  rmSync(goClientSpecPath, { force: true });
}
// The generator still emits the v1 import and an ambiguous tool name.
// Keep generated code on the module's JSON v2 contract and record provenance.
writeFileSync(
  goClientPath,
  readFileSync(goClientPath, "utf8")
    .replace(
      "Code generated by oapi-codegen.",
      "Code generated by github.com/doordash-oss/oapi-codegen-dd/v3.",
    )
    .replaceAll('"encoding/json"', '"encoding/json/v2"'),
);

// Keep the declaration order used by the existing TypeScript client.
const apiDocument = JSON.parse(
  run("go", ["run", "./cmd/agentsview", "openapi"], {
    cwd: repoRoot,
    capture: true,
  }),
);
const rawResponseMediaTypes = new Set([
  "text/event-stream",
  "text/html",
  "text/markdown",
  "application/octet-stream",
  "application/x-tar",
]);
const rawOperations = Object.values(apiDocument.paths).flatMap((path) =>
  Object.values(path)
    .filter(
      (op) =>
        op.responses &&
        Object.values(op.responses).some((response) =>
          Object.keys(response.content ?? {}).some((media) => rawResponseMediaTypes.has(media)),
        ),
    )
    .map((op) => [
      op.operationId,
      {
        mutator: {
          path: join(frontendDir, "src/lib/api/runtime.ts"),
          name: "orvalRequest",
        },
      },
    ]),
);

rmSync(generatedDir, { recursive: true, force: true });
await generate(
  {
    input: { target: apiDocument },
    output: {
      client: "fetch",
      mode: "tags-split",
      target: generatedDir,
      schemas: join(generatedDir, "models"),
      clean: true,
      urlEncodeParameters: true,
      override: {
        fetch: { includeHttpResponseReturnType: false },
        operations: Object.fromEntries(rawOperations),
        // Preserve response bodies for streaming and downloads. Orval still
        // owns each operation's URL, parameters, method, and request encoding.
        transformer: (operation) => {
          const media = operation.response.contentTypes;
          if (media.some((type) => rawResponseMediaTypes.has(type))) {
            operation.response.definition.success = "Response";
          }
          return operation;
        },
        header: () => ["Generated by Orval. Do not edit manually."],
        mutator: {
          path: join(frontendDir, "src/lib/api/runtime.ts"),
          name: "orvalFetch",
        },
        operationName,
        useNamedParameters: true,
      },
    },
  },
  frontendDir,
);
writeIndex();
run("vp", ["fmt", generatedDir], { cwd: frontendDir });
if (verifyGenerated && previousSpec !== readFileSync(specPath, "utf8")) {
  throw new Error("OpenAPI YAML was stale and has been regenerated");
}

if (previousGeneratedSources) {
  const currentGeneratedSources = generatedSources(generatedDir);
  const paths = new Set([...previousGeneratedSources.keys(), ...currentGeneratedSources.keys()]);
  const changed = [...paths].filter(
    (path) => previousGeneratedSources.get(path) !== currentGeneratedSources.get(path),
  );
  if (changed.length > 0) {
    throw new Error(
      `generated API client was stale and has been regenerated:\n${changed.join("\n")}`,
    );
  }
}

if (verifyGenerated && previousGoClient !== readFileSync(goClientPath, "utf8")) {
  throw new Error("Go API client was stale and has been regenerated");
}
