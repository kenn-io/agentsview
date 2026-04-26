export function categoryToken(category: string): string {
  switch (category) {
    case "Read":
    case "Grep":
    case "Glob":
      return "var(--cat-read)";
    case "Edit":
    case "Write":
      return "var(--cat-edit)";
    case "Bash":
      return "var(--cat-bash)";
    case "Task":
      return "var(--cat-task)";
    case "Tool":
      return "var(--cat-tool)";
    case "Mixed":
      return "var(--cat-mixed)";
    default:
      return "var(--cat-other)";
  }
}
