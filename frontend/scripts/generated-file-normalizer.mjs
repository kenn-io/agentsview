import { spawnSync } from "node:child_process";
import { lstatSync, readFileSync, realpathSync, writeFileSync } from "node:fs";
import { isAbsolute, relative, resolve, sep } from "node:path";

function isolatedGitEnvironment() {
  return Object.fromEntries(
    Object.entries(process.env).filter(([name]) => !name.startsWith("GIT_")),
  );
}

function gitPathList(repoRoot, args) {
  const result = spawnSync("git", args, {
    cwd: repoRoot,
    encoding: null,
    env: isolatedGitEnvironment(),
    stdio: ["ignore", "pipe", "pipe"],
  });
  if (result.error) throw result.error;
  if (result.status !== 0) {
    throw new Error(
      `git ${args.join(" ")} exited ${result.status}: ${result.stderr.toString("utf8").trim()}`,
    );
  }
  return result.stdout
    .toString("utf8")
    .split("\0")
    .filter((path) => path !== "");
}

function isWithin(root, path) {
  const child = relative(root, path);
  return child === "" || (!isAbsolute(child) && child !== ".." && !child.startsWith(`..${sep}`));
}

export function normalizeChangedGeneratedFiles(
  repoRoot,
  generatedDir = "frontend/src/lib/api/generated",
) {
  const absoluteRoot = resolve(repoRoot);
  const generatedRoot = resolve(absoluteRoot, generatedDir);
  if (!isWithin(absoluteRoot, generatedRoot)) {
    throw new Error(`generated directory is outside repository: ${generatedDir}`);
  }
  const realGeneratedRoot = realpathSync(generatedRoot);
  const changed = gitPathList(absoluteRoot, [
    "diff",
    "--name-only",
    "-z",
    "--diff-filter=ACMRT",
    "HEAD",
    "--",
    generatedDir,
  ]);
  const untracked = gitPathList(absoluteRoot, [
    "ls-files",
    "-z",
    "--others",
    "--exclude-standard",
    "--",
    generatedDir,
  ]);

  for (const path of new Set([...changed, ...untracked])) {
    if (!path.endsWith(".ts")) continue;
    const absolutePath = resolve(absoluteRoot, path);
    if (!isWithin(generatedRoot, absolutePath)) {
      throw new Error(`generated path is outside generated directory: ${path}`);
    }
    let info;
    try {
      info = lstatSync(absolutePath);
    } catch (error) {
      if (error?.code === "ENOENT") continue;
      throw error;
    }
    if (info.isSymbolicLink()) {
      throw new Error(`generated path is a symbolic link: ${path}`);
    }
    if (!info.isFile()) {
      throw new Error(`generated path is not a regular file: ${path}`);
    }
    const realPath = realpathSync(absolutePath);
    if (!isWithin(realGeneratedRoot, realPath)) {
      throw new Error(`generated path is outside generated directory: ${path}`);
    }
    const source = readFileSync(absolutePath, "utf8");
    writeFileSync(absolutePath, `${source.trimEnd()}\n`);
  }
}
