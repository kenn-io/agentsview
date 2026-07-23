import { spawnSync } from "node:child_process";
import {
  existsSync,
  mkdtempSync,
  mkdirSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import assert from "node:assert/strict";

import { normalizeChangedGeneratedFiles } from "./generated-file-normalizer.mjs";

const generatedDir = "frontend/src/lib/api/generated";

function git(root, ...args) {
  const env = Object.fromEntries(
    Object.entries(process.env).filter(([name]) => !name.startsWith("GIT_")),
  );
  const result = spawnSync("git", args, { cwd: root, encoding: "utf8", env });
  assert.equal(result.status, 0, result.stderr);
}

function writeGenerated(root, name, contents) {
  const path = join(root, generatedDir, name);
  mkdirSync(join(path, ".."), { recursive: true });
  writeFileSync(path, contents);
  return path;
}

function fixture(t) {
  const root = mkdtempSync(join(tmpdir(), "generated-normalizer-test-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  git(root, "init", "--quiet");
  git(root, "config", "user.name", "Normalizer Test");
  git(root, "config", "user.email", "normalizer@example.invalid");
  return root;
}

test("normalizes staged, unstaged, and untracked files while skipping deletions", (t) => {
  const root = fixture(t);
  const staged = writeGenerated(root, "staged.ts", "export const staged = 1;\n");
  const unstaged = writeGenerated(root, "unstaged.ts", "export const unstaged = 1;\n");
  const deleted = writeGenerated(root, "deleted.ts", "export const deleted = 1;\n");
  const untouched = writeGenerated(root, "untouched.ts", "export const untouched = 1;  \n");
  git(root, "add", ".");
  git(root, "commit", "--quiet", "-m", "fixture");

  writeFileSync(staged, "export const staged = 2;  \n\n");
  git(root, "add", staged);
  writeFileSync(unstaged, "export const unstaged = 2;  \n\n");
  rmSync(deleted);
  const untracked = writeGenerated(root, "untracked.ts", "export const untracked = 1;  \n\n");
  const newlineName = writeGenerated(root, "odd\nname.ts", "export const odd = 1;  \n\n");

  normalizeChangedGeneratedFiles(root, generatedDir);

  assert.equal(readFileSync(staged, "utf8"), "export const staged = 2;\n");
  assert.equal(readFileSync(unstaged, "utf8"), "export const unstaged = 2;\n");
  assert.equal(readFileSync(untracked, "utf8"), "export const untracked = 1;\n");
  assert.equal(readFileSync(newlineName, "utf8"), "export const odd = 1;\n");
  assert.equal(readFileSync(untouched, "utf8"), "export const untouched = 1;  \n");
  assert.equal(existsSync(deleted), false);
});

test("rejects generated paths that escape through a symlink", (t) => {
  const root = fixture(t);
  writeGenerated(root, "tracked.ts", "export const tracked = true;\n");
  git(root, "add", ".");
  git(root, "commit", "--quiet", "-m", "fixture");

  const outside = join(root, "outside.ts");
  writeFileSync(outside, "private contents  \n");
  const link = join(root, generatedDir, "escaped.ts");
  symlinkSync(outside, link);

  assert.throws(
    () => normalizeChangedGeneratedFiles(root, generatedDir),
    /symbolic link|outside generated directory/,
  );
  assert.equal(readFileSync(outside, "utf8"), "private contents  \n");
});
