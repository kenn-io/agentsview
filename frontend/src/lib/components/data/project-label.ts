import { m } from "../../i18n/index.js";

export function displayProjectLabel(label: string | null | undefined): string {
  if (label === "unknown") return m.data_project_unclassified();
  return label || m.shared_unknown();
}
