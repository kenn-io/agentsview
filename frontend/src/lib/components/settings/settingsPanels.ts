import type { Component } from "svelte";
import { m } from "../../i18n/index.js";
import {
  CalendarRangeIcon,
  GlobeIcon,
  ImageIcon,
  LanguagesIcon,
  PaletteIcon,
  PlugIcon,
  SparklesIcon,
  SquareTerminalIcon,
} from "../../icons.js";
import GithubIcon from "../shared/GithubIcon.svelte";

export type SettingsPanelId =
  | "appearance"
  | "language"
  | "date-ranges"
  | "terminal"
  | "agent-directories"
  | "tool-result-images"
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
  /** Shown before the label in the settings navigation. */
  icon: Component;
}

export function settingsPanels(): SettingsPanelMeta[] {
  const preferences = m.settings_group_preferences();
  const data = m.settings_group_data();
  const connections = m.settings_group_connections();

  return [
    {
      id: "appearance",
      icon: PaletteIcon,
      label: m.appearance_title(),
      title: m.appearance_title(),
      description: m.appearance_description(),
      group: preferences,
      // The multi-word control labels, so searching a setting by the
      // name it shows finds this panel in every locale.
      keywords: [
        m.settings_search_keywords_appearance(),
        m.appearance_message_layout(),
        m.appearance_reading_width(),
      ].join(" "),
    },
    {
      id: "language",
      icon: LanguagesIcon,
      label: m.settings_language_title(),
      title: m.settings_language_title(),
      description: m.settings_language_description(),
      group: preferences,
      keywords: m.settings_search_keywords_language(),
    },
    {
      id: "date-ranges",
      icon: CalendarRangeIcon,
      label: m.settings_date_ranges_title(),
      title: m.settings_date_ranges_title(),
      description: m.settings_date_ranges_description(),
      group: preferences,
      keywords: m.settings_search_keywords_date_ranges(),
    },
    {
      id: "terminal",
      icon: SquareTerminalIcon,
      label: m.settings_terminal_title(),
      title: m.settings_terminal_title(),
      description: m.settings_terminal_description(),
      group: preferences,
      keywords: m.settings_search_keywords_terminal(),
    },
    {
      id: "agent-directories",
      icon: PlugIcon,
      label: m.settings_session_providers_title(),
      title: m.settings_session_providers_title(),
      description: m.settings_session_providers_description(),
      group: data,
      keywords: m.settings_search_keywords_agent_directories(),
    },
    {
      id: "tool-result-images",
      icon: ImageIcon,
      label: m.settings_tool_images_title(),
      title: m.settings_tool_images_title(),
      description: m.settings_tool_images_description(),
      group: data,
      keywords: m.settings_search_keywords_tool_images(),
    },
    {
      id: "embeddings",
      icon: SparklesIcon,
      label: m.settings_embeddings_title(),
      title: m.settings_embeddings_title(),
      description: m.settings_embeddings_description(),
      group: data,
      keywords: m.settings_search_keywords_embeddings(),
    },
    {
      id: "github",
      icon: GithubIcon,
      label: m.settings_nav_github(),
      title: m.settings_github_title(),
      description: m.settings_github_description(),
      group: connections,
      keywords: m.settings_search_keywords_github(),
    },
    {
      id: "remote-access",
      icon: GlobeIcon,
      label: m.settings_nav_remote_access(),
      title: m.settings_remote_title(),
      description: m.settings_remote_description(),
      group: connections,
      keywords: m.settings_search_keywords_remote_access(),
    },
  ];
}
