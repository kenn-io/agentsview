---
name: sync-harness-icons
description: Use when a person asks to audit or update the harness marks after new agent parsers were added, when the landing-page agent grid or the kit-ui HarnessIcon set is missing a newly supported agent, or when someone invokes /sync-harness-icons. Human-invoked only.
disable-model-invocation: true
---

# Sync Harness Icons

Every harness AgentsView can read gets a monochrome brand mark in two places:
the "Every agent" chip grid in `docs/website/index.html`, and the `HarnessIcon`
component in the kit-ui repository that Forge draws in its launch and terminal
surfaces. New parsers land without either, so a person runs this skill after a
batch of provider additions to bring both back in sync, then moves the Forge pin
so Forge picks the marks up. Do the steps in order; each pull request references
the one before it.

## 1. Audit

From the AgentsView repository root, with a kit-ui checkout that is on `main`
and up to date (`git -C <kit-ui> status -sb` shows `## main...origin/main`):

```bash
python3 .agents/skills/sync-harness-icons/audit.py --kit-ui <kit-ui checkout>
```

The script reads the provider headings in
`docs/internal/session-format-sources.md`, so a new parser is invisible to the
audit until its heading and evidence entry exist there; confirm that first. It
reports:

- providers with no landing-grid chip, matched by normalized name against the
  chip list. Judge each line. Importers and IDE/CLI variants of a brand
  already on the grid stay off it; add such keys to `INTENTIONALLY_UNLISTED`
  in the script with the reason. A provider the matcher missed because its
  chip has a different display name needs no action; note it in the report.
  Everything else needs a mark.
- sprite symbols with no kit-ui id, which is the kit-ui gap list.
- chips whose name appears in no kit-ui `agents` list. A chip that uses a text
  tag instead of a glyph (IcodeMate) is expected here and needs no action.

## 2. Source a mark for each missing harness

Dispatch one subagent per three or four harnesses with this brief, writing to a
scratch directory outside any repository:

- Find the vendor repository from the provider's entry in
  `docs/internal/session-format-sources.md`; if the entry has no repository,
  search the product site. Look for a first-party vector: `assets/`, `docs/`,
  `.github/`, the README header image, `favicon.svg`, a VS Code or JetBrains
  extension icon.
- Fallbacks in order: the lobehub monochrome redraw
  (`npm pack @lobehub/icons-static-svg`, file `icons/<name>.svg`) when a
  side-by-side render matches the official mark's silhouette; the publisher's
  mark when the product has no distinct logo (this is how Codex draws OpenAI
  and QClaw draws Tencent); potrace of an official raster of at least 512 px,
  stated as such. Never hand-draw.
- Normalize: parse the source, compute the mark's bounding box, then scale and
  translate it into a 20-unit box centred at (12,12) on `viewBox="0 0 24 24"`,
  either by rewriting the path coordinates or by wrapping in
  `<g transform="…">` (the sprite already contains both styles). Use
  `fill-rule="evenodd"` for holes. Strip fill and stroke colours, gradients,
  masks, clip paths, filters, and `<style>`. Valid XML, under 4 KB.
- Render with `rsvg-convert` at 64 px and 16 px as a light glyph on a dark
  background (add a temporary `fill="#fff"` for the preview only) and view the
  PNGs.
- Report a table: agent, slug, source URL, licence, confidence, and anything
  that has no usable vector. A harness with no findable logo keeps a
  two-letter text tag, as IcodeMate does.

Look at every rendered PNG yourself before using a mark. Same-hue accent pixels
and thin decorative rings vanish in monochrome; drop them rather than keep an
unreadable silhouette.

## 3. Add to the landing grid

1. Minify: `npx --yes svgo@3 --multipass -q -o <out> -f <scratch>`.

1. Wrap each file's inner markup as
   `<symbol id="i-<slug>" viewBox="0 0 24 24">…</symbol>` and insert it after
   the last existing `<symbol>` line. A publisher fallback reuses the existing
   symbol id instead of adding one.

1. Add a chip. The grid opens with a fixed run of chips that ends at Zencoder;
   the chips after Zencoder are roughly alphabetical, so place the new chip by
   name among them. Use the display name from the README session discovery
   table:

    ```html
    <a class="agent-chip" href="/docs/configuration/#session-discovery"><span class="agent-chip-glyph"><svg viewBox="0 0 24 24" aria-hidden="true"><use xlink:href="#i-<slug>"/></svg></span><span class="agent-chip-name"><Name></span></a>
    ```

    Keep `xlink:href`; the built-site check requires it.

1. Verify: `make docs-check`, then

    ```bash
    (cd docs/website && python3 -m http.server 8766 --bind 127.0.0.1 &)
    "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" --headless=new --hide-scrollbars \
      --window-size=1280,2000 --screenshot=/tmp/grid.png http://127.0.0.1:8766/index.html
    ```

    Crop to the grid and show the person the image.

1. Rerun the audit; the grid list must be empty. Commit and open the pull
   request with `kenn:commit-push-pr`. The PR body lists where each mark came
   from and its licence, since the diff cannot show that.

## 4. Mirror into kit-ui

Work in a fresh worktree from kit-ui `main`. One commit touches:

| File                                      | Change                                                                                                                                       |
| ----------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------- |
| `src/lib/assets/harness-icons/<slug>.svg` | The minified file with the same path data as the sprite; `bun run fmt` settles tag spacing                                                   |
| `src/lib/components/harness-icon.ts`      | Append to the `HarnessIconId` union and `HARNESS_ICONS`, after the last entry: `{ id: "<slug>", label: "<Brand>", agents: ["<chip name>"] }` |
| `src/lib/components/HarnessIcon.svelte`   | `import <slug> from "../assets/harness-icons/<slug>.svg?raw"` and add `<slug>,` to `GLYPHS`                                                  |
| `docs/components/harness-icon.md`         | A row in the Harnesses table: id in backticks, brand, then the agent names comma separated, in the same three columns as the rows above it   |
| `tests/browser/harness-icon.spec.ts`      | Raise both `toHaveCount` values by the number added                                                                                          |

Run `bun install`, `bun run fmt`, `bun run check`, `bun run lint`,
`bun run test:browser`, and `bun run build`. Screenshot the HarnessIcon page of
the demo gallery (`bun run dev -- --port <n>`, then `/#harness-icon` with the
same headless Chrome command). Open the PR; its body references the AgentsView
PR for provenance.

## 5. Move the Forge pin

After the kit-ui PR merges, get the merge commit:

```bash
gh pr view <n> --repo kenn-io/kit-ui --json mergeCommit -q .mergeCommit.oid
```

In a worktree from Forge `main`: replace the commit hash in the
`@kenn-io/kit-ui` entry of `frontend/package.json` with that hash, run
`bun install`, confirm the new SVG files exist under
`frontend/node_modules/@kenn-io/kit-ui/src/lib/assets/harness-icons/`, run
`make frontend-check-core-no-deps`, then from `frontend/` run
`node ../node_modules/vite-plus/bin/vp test run --project unit-node --project unit-jsdom agentHarness`,
and open a PR that commits `frontend/package.json` and the repository-root
`bun.lock`. Forge matches launch target keys against the registry's `agents`
names, so a new harness whose key is its name needs no Forge code change. Never
pin to the PR head; it is deleted after the squash merge.

AgentsView's own frontend does not import `HarnessIcon`; its kit-ui pin needs no
bump for icon work.

## Common mistakes

- Trusting the audit's list without reading it: it matches by name and cannot
  know a new key is a variant of an existing brand.
- A mark that renders black on a dark preview: the file still carries a fill
  colour, or the preview omitted one.
- Skipping the private-terms scrub before push. SVG path data trips the
  IP-address heuristic, which is a false positive, but a private repository
  name in a comment is not.
