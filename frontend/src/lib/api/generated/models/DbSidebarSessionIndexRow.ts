/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
export type DbSidebarSessionIndexRow = {
  agent: string;
  created_at: string;
  display_name?: string;
  ended_at: string | null;
  file_hash?: string;
  id: string;
  is_automated: boolean;
  is_teammate: boolean;
  local_modified_at?: string;
  machine: string;
  message_count: number;
  parent_session_id?: string;
  project: string;
  relationship_type?: string;
  started_at: string | null;
  termination_status?: string;
  user_message_count: number;
};
