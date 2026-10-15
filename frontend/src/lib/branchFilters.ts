// Keep these in sync with internal/db branch token separators.
export const BRANCH_TOKEN_SEP = "\u001f";
export const BRANCH_LIST_SEP = "\u001e";
export const NO_BRANCH_FILTER_TOKEN = "\u001dno_branch";
export const NO_BRANCH_MATCH_TOKEN = "\u001dno_branch_match";
const OPAQUE_PROJECT_KEY_PREFIX = "pl1:sha256:";

export function branchFilterToken(project: string, branch: string): string {
  return project + BRANCH_TOKEN_SEP + branch;
}

export function splitBranchFilterToken(token: string): {
  project: string;
  branch: string;
} {
  const i = token.indexOf(BRANCH_TOKEN_SEP);
  return i < 0
    ? { project: "", branch: token }
    : { project: token.slice(0, i), branch: token.slice(i + 1) };
}

export function branchLabel(project: string, branch: string, noBranchLabel: string): string {
  const label = branch || noBranchLabel;
  return project ? `${project}/${label}` : label;
}

export function branchTokenLabel(token: string, noBranchLabel: string): string {
  if (token === NO_BRANCH_FILTER_TOKEN) return noBranchLabel;
  const { project, branch } = splitBranchFilterToken(token);
  return branchLabel(project, branch, noBranchLabel);
}

export function branchFilterValue(value: string): string {
  const name = branchName(value);
  return name === "" ? NO_BRANCH_FILTER_TOKEN : name;
}

function branchName(value: string): string {
  if (value === NO_BRANCH_FILTER_TOKEN) return "";
  return splitBranchFilterToken(value).branch;
}

// Resolves an opaque project key to the raw project label it stands for.
export type ProjectLabelLookup = (projectKey: string) => string | undefined;

export function isOpaqueProjectKey(project: string): boolean {
  return project.startsWith(OPAQUE_PROJECT_KEY_PREFIX);
}

// Legacy URLs qualify branches by raw project label while usage rows use
// opaque keys. Each key stands for exactly one label, so a key and a label
// name the same project only when the key resolves to that label.
function sameProject(left: string, right: string, projectLabel?: ProjectLabelLookup): boolean {
  if (!left || !right || left === right) return true;
  const leftOpaque = isOpaqueProjectKey(left);
  if (!projectLabel || leftOpaque === isOpaqueProjectKey(right)) return false;
  return leftOpaque ? projectLabel(left) === right : projectLabel(right) === left;
}

export function branchFilterValuesEqual(
  left: string,
  right: string,
  projectLabel?: ProjectLabelLookup,
): boolean {
  if (branchName(left) !== branchName(right)) return false;
  return sameProject(
    splitBranchFilterToken(left).project,
    splitBranchFilterToken(right).project,
    projectLabel,
  );
}

export function branchPickerValues(values: string[]): string[] {
  return [...new Set(values.map(branchFilterValue))];
}

export function portableBranchFilterValues(values: string[]): string[] {
  return [
    ...new Set(
      values.map((value) => {
        const { project } = splitBranchFilterToken(value);
        return isOpaqueProjectKey(project) ? branchFilterValue(value) : value;
      }),
    ),
  ];
}

export function reconcileBranchFilterValues(current: string[], pickerValues: string[]): string[] {
  const selected = new Set(pickerValues.map(branchFilterValue));
  const next: string[] = [];
  const seen = new Set<string>();
  const represented = new Set<string>();
  for (const value of current) {
    const name = branchFilterValue(value);
    if (!selected.has(name) || seen.has(value)) continue;
    seen.add(value);
    represented.add(name);
    next.push(value);
  }
  for (const value of pickerValues) {
    const name = branchFilterValue(value);
    if (represented.has(name) || seen.has(name)) continue;
    seen.add(name);
    represented.add(name);
    next.push(name);
  }
  return next;
}

export function scopeBranchFilterValues(values: string[], project: string): string[] {
  if (!project) return values;
  return values.filter((value) => {
    const decoded = splitBranchFilterToken(value);
    return !decoded.project || decoded.project === project || isOpaqueProjectKey(decoded.project);
  });
}

export function intersectBranchFilterValues(
  left: string[],
  right: string[],
  projectLabel?: ProjectLabelLookup,
): string[] {
  const result: string[] = [];
  const seen = new Set<string>();
  for (const leftValue of left) {
    const leftToken = splitBranchFilterToken(leftValue);
    for (const rightValue of right) {
      if (!branchFilterValuesEqual(leftValue, rightValue, projectLabel)) continue;
      const value = leftToken.project ? leftValue : rightValue;
      if (!seen.has(value)) {
        seen.add(value);
        result.push(value);
      }
    }
  }
  return result;
}
