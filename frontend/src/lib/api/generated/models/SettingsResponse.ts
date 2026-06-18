/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { TerminalResponse } from './TerminalResponse';
export type SettingsResponse = {
  agent_dirs: Record<string, any[] | null>;
  auth_token?: string;
  github_configured: boolean;
  host: string;
  port: number;
  read_only: boolean;
  require_auth: boolean;
  terminal: TerminalResponse;
};

