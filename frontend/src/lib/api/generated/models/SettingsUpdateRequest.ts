/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { TerminalResponse } from './TerminalResponse';
export type SettingsUpdateRequest = {
  auth_token?: string;
  chart_palette?: string;
  disabled_agents?: Array<string>;
  require_auth?: boolean;
  terminal?: TerminalResponse;
};

