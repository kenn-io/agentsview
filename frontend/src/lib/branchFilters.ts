// Keep these in sync with internal/db branch token separators.
const BRANCH_TOKEN_SEP = "\u001f";

export function branchFilterToken(project: string, branch: string): string {
  return project + BRANCH_TOKEN_SEP + branch;
}

function splitBranchFilterToken(token: string): {
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
  const { project, branch } = splitBranchFilterToken(token);
  return branchLabel(project, branch, noBranchLabel);
}
