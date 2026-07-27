import { m } from "../../i18n/index.js";

export type SettingsPanelId =
  | "appearance"
  | "language"
  | "date-ranges"
  | "terminal"
  | "agent-directories"
  | "worktree-mappings"
  | "embeddings"
  | "github"
  | "remote-access";

export interface SettingsPanelMeta {
  id: SettingsPanelId;
  label: string;
  title: string;
  description: string;
  group: string;
  keywords: string;
}

export function settingsPanels(): SettingsPanelMeta[] {
  const preferences = m.settings_group_preferences();
  const data = m.settings_group_data();
  const connections = m.settings_group_connections();

  return [
    {
      id: "appearance",
      label: m.appearance_title(),
      title: m.appearance_title(),
      description: m.appearance_description(),
      group: preferences,
      keywords: "theme contrast layout text font blocks",
    },
    {
      id: "language",
      label: m.settings_language_title(),
      title: m.settings_language_title(),
      description: m.settings_language_description(),
      group: preferences,
      keywords: "language locale translation",
    },
    {
      id: "date-ranges",
      label: m.settings_date_ranges_title(),
      title: m.settings_date_ranges_title(),
      description: m.settings_date_ranges_description(),
      group: preferences,
      keywords: "date range linked pages",
    },
    {
      id: "terminal",
      label: m.settings_terminal_title(),
      title: m.settings_terminal_title(),
      description: m.settings_terminal_description(),
      group: preferences,
      keywords: "terminal resume launch binary arguments clipboard",
    },
    {
      id: "agent-directories",
      label: m.settings_agent_dir_title(),
      title: m.settings_agent_dir_title(),
      description: m.settings_agent_dir_description(),
      group: data,
      keywords: "agent directories paths scan sessions",
    },
    {
      id: "worktree-mappings",
      label: m.worktree_title(),
      title: m.worktree_title(),
      description: m.worktree_description(),
      group: data,
      keywords: "worktree mappings projects paths layouts",
    },
    {
      id: "embeddings",
      label: m.settings_embeddings_title(),
      title: m.settings_embeddings_title(),
      description: m.settings_embeddings_description(),
      group: data,
      keywords: "embeddings semantic search vectors index generations",
    },
    {
      id: "github",
      label: m.settings_nav_github(),
      title: m.settings_github_title(),
      description: m.settings_github_description(),
      group: connections,
      keywords: "github gist token publish integration",
    },
    {
      id: "remote-access",
      label: m.settings_nav_remote_access(),
      title: m.settings_remote_title(),
      description: m.settings_remote_description(),
      group: connections,
      keywords: "remote server connection auth token network",
    },
  ];
}
