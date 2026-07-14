// Keep these in sync with internal/db branch token separators.
export const BRANCH_TOKEN_SEP = "\u001f";
export const BRANCH_LIST_SEP = "\u001e";
export const NO_BRANCH_FILTER_TOKEN = "\u001dno_branch";
export const NO_BRANCH_MATCH_TOKEN = "\u001dno_branch_match";

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

export function branchPickerValues(values: string[]): string[] {
  return [...new Set(values.map(branchFilterValue))];
}

export function reconcileBranchFilterValues(
  current: string[],
  pickerValues: string[],
): string[] {
  const remaining = new Set(pickerValues.map(branchFilterValue));
  const next: string[] = [];
  for (const value of current) {
    const name = branchFilterValue(value);
    if (!remaining.delete(name)) continue;
    next.push(value);
  }
  for (const value of pickerValues) {
    const name = branchFilterValue(value);
    if (!remaining.delete(name)) continue;
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
    return !decoded.project || decoded.project === project;
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
      const rightToken = splitBranchFilterToken(rightValue);
      if (branchName(leftValue) !== branchName(rightValue)) continue;
      if (
        leftToken.project &&
        rightToken.project &&
        leftToken.project !== rightToken.project
      ) continue;
      const value = leftToken.project ? leftValue : rightValue;
      if (!seen.has(value)) {
        seen.add(value);
        result.push(value);
      }
      break;
    }
  }
  return result;
}
