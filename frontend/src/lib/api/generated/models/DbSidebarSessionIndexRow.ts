/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
export type DbSidebarSessionIndexRow = {
  agent: string;
  agent_label?: string;
  created_at: string;
  display_name?: string;
  ended_at: string | null;
  entrypoint?: string;
  id: string;
  is_automated: boolean;
  is_teammate: boolean;
  machine: string;
  message_count: number;
  parent_session_id?: string;
  project: string;
  relationship_type?: string;
  started_at: string | null;
  termination_status?: string;
  transcript_revision?: string;
  user_message_count: number;
};

