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
      keywords: m.settings_search_keywords_appearance(),
    },
    {
      id: "language",
      label: m.settings_language_title(),
      title: m.settings_language_title(),
      description: m.settings_language_description(),
      group: preferences,
      keywords: m.settings_search_keywords_language(),
    },
    {
      id: "date-ranges",
      label: m.settings_date_ranges_title(),
      title: m.settings_date_ranges_title(),
      description: m.settings_date_ranges_description(),
      group: preferences,
      keywords: m.settings_search_keywords_date_ranges(),
    },
    {
      id: "terminal",
      label: m.settings_terminal_title(),
      title: m.settings_terminal_title(),
      description: m.settings_terminal_description(),
      group: preferences,
      keywords: m.settings_search_keywords_terminal(),
    },
    {
      id: "agent-directories",
      label: m.settings_session_providers_title(),
      title: m.settings_session_providers_title(),
      description: m.settings_session_providers_description(),
      group: data,
      keywords: m.settings_search_keywords_agent_directories(),
    },
    {
      id: "worktree-mappings",
      label: m.worktree_title(),
      title: m.worktree_title(),
      description: m.settings_worktree_moved(),
      group: data,
      keywords: m.settings_search_keywords_worktree_mappings(),
    },
    {
      id: "embeddings",
      label: m.settings_embeddings_title(),
      title: m.settings_embeddings_title(),
      description: m.settings_embeddings_description(),
      group: data,
      keywords: m.settings_search_keywords_embeddings(),
    },
    {
      id: "github",
      label: m.settings_nav_github(),
      title: m.settings_github_title(),
      description: m.settings_github_description(),
      group: connections,
      keywords: m.settings_search_keywords_github(),
    },
    {
      id: "remote-access",
      label: m.settings_nav_remote_access(),
      title: m.settings_remote_title(),
      description: m.settings_remote_description(),
      group: connections,
      keywords: m.settings_search_keywords_remote_access(),
    },
  ];
}
