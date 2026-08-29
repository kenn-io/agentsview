/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
export type DbMessage = {
  claude_message_id?: string;
  claude_request_id?: string;
  content: string;
  content_length: number;
  context_tokens: number;
  has_context_tokens: boolean;
  has_output_tokens: boolean;
  has_thinking: boolean;
  has_tool_use: boolean;
  id: number;
  is_compact_boundary?: boolean;
  is_sidechain?: boolean;
  is_system: boolean;
  model: string;
  ordinal: number;
  output_tokens: number;
  prompt_source?: string;
  provider_id?: string;
  role: string;
  session_id: string;
  source_parent_uuid?: string;
  source_subtype?: string;
  source_type?: string;
  source_uuid?: string;
  thinking_text: string;
  timestamp: string;
  token_usage?: any;
  tool_calls?: any[] | null;
};
