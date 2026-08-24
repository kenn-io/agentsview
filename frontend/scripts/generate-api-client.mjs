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

function preserveStructuredResponseHeaders() {
  const optionsPath = join(frontendDir, "src/lib/api/generated/core/ApiRequestOptions.ts");
  const optionsSource = readFileSync(optionsPath, "utf8");
  const generatedOption = "  readonly responseHeader?: string;\n";
  const structuredOption = `  readonly responseHeader?: string;
  readonly responseHeaders?: Record<string, {
    readonly name: string;
    readonly type: 'string' | 'number' | 'boolean';
  }>;
`;
  if (!optionsSource.includes(generatedOption)) {
    throw new Error("generated request options no longer match the response-header patch");
  }
  writeFileSync(optionsPath, optionsSource.replace(generatedOption, structuredOption));

  const requestPath = join(frontendDir, "src/lib/api/generated/core/request.ts");
  let requestSource = readFileSync(requestPath, "utf8");
  const generatedHeaderReader = `export const getResponseHeader = (response: Response, responseHeader?: string): string | undefined => {
  if (responseHeader) {
    const content = response.headers.get(responseHeader);
    if (isString(content)) {
      return content;
    }
  }
  return undefined;
};
`;
  const structuredHeaderReader = `${generatedHeaderReader}
export const getResponseHeaders = (
  response: Response,
  responseHeaders?: ApiRequestOptions['responseHeaders'],
): Record<string, string | number | boolean> | undefined => {
  if (!responseHeaders) return undefined;
  return Object.fromEntries(Object.entries(responseHeaders).map(([property, option]) => {
    const content = response.headers.get(option.name);
    if (content === null) throw new Error(\`Response header \${option.name} is missing\`);
    switch (option.type) {
      case 'number': {
        const value = Number(content);
        if (!Number.isFinite(value)) {
          throw new Error(\`Response header \${option.name} is not a number\`);
        }
        return [property, value];
      }
      case 'boolean':
        if (content === 'true') return [property, true];
        if (content === 'false') return [property, false];
        throw new Error(\`Response header \${option.name} is not a boolean\`);
      default:
        return [property, content];
    }
  }));
};
`;
  if (!requestSource.includes(generatedHeaderReader)) {
    throw new Error("generated request helper no longer matches the response-header patch");
  }
  requestSource = requestSource.replace(generatedHeaderReader, structuredHeaderReader);
  const generatedResponseRead = `        const responseBody = await getResponseBody(response);
        const responseHeader = getResponseHeader(response, options.responseHeader);

        const result: ApiResult = {
          url,
          ok: response.ok,
          status: response.status,
          statusText: response.statusText,
          body: responseHeader ?? responseBody,
        };
`;
  const structuredResponseRead = `        const responseBody = await getResponseBody(response);
        const responseHeader = getResponseHeader(response, options.responseHeader);
        const responseHeaders = response.ok
          ? getResponseHeaders(response, options.responseHeaders)
          : undefined;

        const result: ApiResult = {
          url,
          ok: response.ok,
          status: response.status,
          statusText: response.statusText,
          body: responseHeaders ?? responseHeader ?? responseBody,
        };
`;
  if (!requestSource.includes(generatedResponseRead)) {
    throw new Error("generated request flow no longer matches the response-header patch");
  }
  writeFileSync(requestPath, requestSource.replace(generatedResponseRead, structuredResponseRead));
}

function preserveRawSyncUploadStatusResponse() {
  const modelName = "RawSyncUploadStatusResponse";
  const modelPath = join(frontendDir, `src/lib/api/generated/models/${modelName}.ts`);
  writeFileSync(modelPath, `/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
export type ${modelName} = {
  offset: number;
  length: number;
  complete: boolean;
};
`);

  const indexPath = join(frontendDir, "src/lib/api/generated/index.ts");
  const indexSource = readFileSync(indexPath, "utf8");
  const responseExport = "export type { RawSyncUploadResponse } from './models/RawSyncUploadResponse';\n";
  if (!indexSource.includes(responseExport)) {
    throw new Error("generated API index no longer matches the raw upload status patch");
  }
  writeFileSync(
    indexPath,
    indexSource.replace(
      responseExport,
      `${responseExport}export type { ${modelName} } from './models/${modelName}';\n`,
    ),
  );

  const servicePath = join(frontendDir, "src/lib/api/generated/services/RawSyncService.ts");
  let serviceSource = readFileSync(servicePath, "utf8");
  const responseImport =
    "import type { RawSyncUploadResponse } from '../models/RawSyncUploadResponse';\n";
  if (!serviceSource.includes(responseImport)) {
    throw new Error("generated raw sync imports no longer match the upload status patch");
  }
  serviceSource = serviceSource.replace(
    responseImport,
    `${responseImport}import type { ${modelName} } from '../models/${modelName}';\n`,
  );
  const statusStart = serviceSource.indexOf("   * Read a raw upload offset");
  const statusEnd = serviceSource.indexOf("  /**", statusStart + 1);
  if (statusStart < 0 || statusEnd < 0) {
    throw new Error("generated raw upload status method was not found");
  }
  let statusMethod = serviceSource.slice(statusStart, statusEnd);
  const generatedDocumentation = "   * @returns string OK";
  const generatedReturn = "  }): CancelablePromise<string> {";
  const generatedHeader = "      responseHeader: 'Upload-Complete',";
  if (
    !statusMethod.includes(generatedDocumentation) ||
    !statusMethod.includes(generatedReturn) ||
    !statusMethod.includes(generatedHeader)
  ) {
    throw new Error("generated raw upload status method no longer matches the header patch");
  }
  statusMethod = statusMethod
    .replace(generatedDocumentation, `   * @returns ${modelName} OK`)
    .replace(generatedReturn, `  }): CancelablePromise<${modelName}> {`)
    .replace(generatedHeader, `      responseHeaders: {
        offset: { name: 'Upload-Offset', type: 'number' },
        length: { name: 'Upload-Length', type: 'number' },
        complete: { name: 'Upload-Complete', type: 'boolean' },
      },`);
  writeFileSync(
    servicePath,
    serviceSource.slice(0, statusStart) + statusMethod + serviceSource.slice(statusEnd),
  );
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
  preserveStructuredResponseHeaders();
  preserveRawSyncUploadStatusResponse();
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
