function enabled(value: string | undefined): boolean {
  return value === "1" || value === "true";
}

export const PROJECT_MAPPING_WORKSPACE_ENABLED = enabled(
  import.meta.env.VITE_PROJECT_MAPPING_WORKSPACE,
);
