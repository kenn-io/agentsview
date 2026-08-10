/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { SessionProviderResponse } from './SessionProviderResponse';
import type { TerminalResponse } from './TerminalResponse';
export type SettingsResponse = {
  agent_dirs: Record<string, any[] | null>;
  auth_token?: string;
  chart_palette: string;
  disabled_agents: Array<string> | null;
  github_configured: boolean;
  host: string;
  port: number;
  read_only: boolean;
  require_auth: boolean;
  session_providers: Array<SessionProviderResponse> | null;
  terminal: TerminalResponse;
};

