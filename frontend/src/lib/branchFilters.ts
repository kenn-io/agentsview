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

export function branchLabel(
  project: string,
  branch: string,
  noBranchLabel: string,
): string {
  const label = branch || noBranchLabel;
  return project ? `${project}/${label}` : label;
}

export function branchTokenLabel(
  token: string,
  noBranchLabel: string,
): string {
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

export function branchFilterValuesEqual(
  left: string,
  right: string,
): boolean {
  const leftToken = splitBranchFilterToken(left);
  const rightToken = splitBranchFilterToken(right);
  if (branchName(left) !== branchName(right)) return false;
  return !leftToken.project ||
    !rightToken.project ||
    leftToken.project === rightToken.project;
}

export function branchPickerValues(values: string[]): string[] {
  return [...new Set(values.map(branchFilterValue))];
}

export function portableBranchFilterValues(values: string[]): string[] {
  return [...new Set(values.map((value) => {
    const { project } = splitBranchFilterToken(value);
    return project.startsWith(OPAQUE_PROJECT_KEY_PREFIX)
      ? branchFilterValue(value)
      : value;
  }))];
}

export function reconcileBranchFilterValues(
  current: string[],
  pickerValues: string[],
): string[] {
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

export function scopeBranchFilterValues(
  values: string[],
  project: string,
): string[] {
  if (!project) return values;
  return values.filter((value) => {
    const decoded = splitBranchFilterToken(value);
    return !decoded.project ||
      decoded.project === project ||
      decoded.project.startsWith(OPAQUE_PROJECT_KEY_PREFIX);
  });
}

export function intersectBranchFilterValues(
  left: string[],
  right: string[],
): string[] {
  const result: string[] = [];
  const seen = new Set<string>();
  for (const leftValue of left) {
    const leftToken = splitBranchFilterToken(leftValue);
    for (const rightValue of right) {
      if (!branchFilterValuesEqual(leftValue, rightValue)) continue;
      const value = leftToken.project ? leftValue : rightValue;
      if (!seen.has(value)) {
        seen.add(value);
        result.push(value);
      }
    }
  }
  return result;
}
