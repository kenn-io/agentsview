-- Captured from OpenCode 0.0.0-beta-19381; scratch paths replaced.

CREATE TABLE `project` (
          `id` text PRIMARY KEY,
          `worktree` text NOT NULL,
          `vcs` text,
          `name` text,
          `icon_url` text,
          `icon_url_override` text,
          `icon_color` text,
          `time_created` integer NOT NULL,
          `time_updated` integer NOT NULL,
          `time_initialized` integer,
          `sandboxes` text NOT NULL,
          `commands` text
        );

INSERT INTO project VALUES ('46959d5a6b1f1f358e3bb9ee11816810d84ed66e', '/workspace/project-a', NULL, NULL, NULL, NULL, NULL, 1788972143306, 1788972143306, NULL, '[]', NULL);

CREATE TABLE `session_v2` (
          `id` text PRIMARY KEY,
          `project_id` text NOT NULL,
          `workspace_id` text,
          `parent_id` text,
          `fork_session_id` text,
          `fork_boundary` text,
          `slug` text NOT NULL,
          `directory` text NOT NULL,
          `path` text,
          `title` text,
          `version` text NOT NULL,
          `share_url` text,
          `summary_additions` integer,
          `summary_deletions` integer,
          `summary_files` integer,
          `summary_diffs` text,
          `metadata` text,
          `cost` real DEFAULT 0 NOT NULL,
          `tokens_input` integer DEFAULT 0 NOT NULL,
          `tokens_output` integer DEFAULT 0 NOT NULL,
          `tokens_reasoning` integer DEFAULT 0 NOT NULL,
          `tokens_cache_read` integer DEFAULT 0 NOT NULL,
          `tokens_cache_write` integer DEFAULT 0 NOT NULL,
          `revert` text,
          `permission` text,
          `agent` text,
          `model` text,
          `time_created` integer NOT NULL,
          `time_updated` integer NOT NULL,
          `time_idle` integer,
          `time_viewed` integer,
          `idle_outcome` text,
          `time_compacting` integer,
          `time_archived` integer,
          `time_suspended` integer,
          `resume_attempts` integer DEFAULT 0 NOT NULL,
          CONSTRAINT `fk_session_v2_project_id_project_id_fk` FOREIGN KEY (`project_id`) REFERENCES `project`(`id`) ON DELETE CASCADE
        );

INSERT INTO session_v2 VALUES ('ses_f78f36bd6ffeTXo4UWv9dIETyI', '46959d5a6b1f1f358e3bb9ee11816810d84ed66e', NULL, NULL, NULL, NULL, 'hidden-panda', '/workspace/project-a', '', NULL, '0.0.0-beta-19381', NULL, NULL, NULL, NULL, NULL, NULL, 0.0, 6884, 25, 0, 0, 0, NULL, NULL, NULL, '{"id":"big-pickle","providerID":"opencode"}', 1788972143734, 1788972143771, 1788972150408, NULL, 'succeeded', NULL, NULL, NULL, 0);

INSERT INTO session_v2 VALUES ('ses_f78f34fafffeIugr4PB2oXkVD0', '46959d5a6b1f1f358e3bb9ee11816810d84ed66e', NULL, NULL, NULL, NULL, 'cosmic-moon', '/workspace/project-a', '', NULL, '0.0.0-beta-19381', NULL, NULL, NULL, NULL, NULL, NULL, 0.0, 7237, 174, 0, 13888, 0, NULL, NULL, NULL, '{"id":"big-pickle","providerID":"opencode"}', 1788972151032, 1788972156981, 1788972159327, NULL, 'succeeded', NULL, NULL, NULL, 0);

INSERT INTO session_v2 VALUES ('ses_f78ee770effebwbWXea2fzwUIu', '46959d5a6b1f1f358e3bb9ee11816810d84ed66e', NULL, NULL, NULL, NULL, 'sunny-river', '/workspace/project-a', '', NULL, '0.0.0-beta-19381', NULL, NULL, NULL, NULL, NULL, NULL, 0.0, 7026, 96, 0, 6848, 0, NULL, NULL, NULL, '{"id":"big-pickle","providerID":"opencode"}', 1788972468558, 1788972468594, 1788972476190, NULL, 'succeeded', NULL, NULL, NULL, 0);

CREATE INDEX `session_v2_project_idx` ON `session_v2` (`project_id`);

CREATE INDEX `session_v2_workspace_idx` ON `session_v2` (`workspace_id`);

CREATE INDEX `session_v2_parent_idx` ON `session_v2` (`parent_id`);

CREATE INDEX `session_v2_time_suspended_idx` ON `session_v2` (`time_suspended`) WHERE "session_v2"."time_suspended" is not null;

CREATE TABLE `session_message` (
          `id` text PRIMARY KEY,
          `session_id` text NOT NULL,
          `type` text NOT NULL,
          `seq` integer NOT NULL,
          `time_created` integer NOT NULL,
          `time_updated` integer NOT NULL,
          `data` text NOT NULL,
          CONSTRAINT `fk_session_message_session_id_session_v2_id_fk` FOREIGN KEY (`session_id`) REFERENCES `session_v2`(`id`) ON DELETE CASCADE
        );

INSERT INTO session_message VALUES ('msg_0870c947c001u289U4KpLcgWyg', 'ses_f78f36bd6ffeTXo4UWv9dIETyI', 'user', 4, 1788972143788, 1788972143789, '{"time":{"created":1788972143788},"text":"\"Reply with exactly: beta parser check. Do not use tools.\"","files":[]}');

INSERT INTO session_message VALUES ('msg_0870c94b5001MOCHPdKgMwlaXJ', 'ses_f78f36bd6ffeTXo4UWv9dIETyI', 'assistant', 5, 1788972144865, 1788972150403, '{"time":{"created":1788972144865,"streamed":1788972150400,"completed":1788972150402},"agent":"build","model":{"id":"big-pickle","providerID":"opencode","variant":"default"},"content":[{"type":"reasoning","text":"The user is asking me to reply with exactly \"beta parser check\" and not use tools.","state":{"reasoningField":"reasoning_content"},"time":{"created":1788972149973,"completed":1788972150396}},{"type":"text","text":"beta parser check"}],"finish":"stop","rawFinish":"stop","cost":0,"tokens":{"input":6884,"output":25,"reasoning":0,"cache":{"read":0,"write":0}}}');

INSERT INTO session_message VALUES ('msg_0870cb136001itPj1xDGbAXBug', 'ses_f78f34fafffeIugr4PB2oXkVD0', 'user', 4, 1788972151133, 1788972151134, '{"time":{"created":1788972151133},"text":"\"Read input.txt using the read tool. Reply with the three words in order and the number of lines. Do not edit files or run other commands.\"","files":[]}');

INSERT INTO session_message VALUES ('msg_0870cb168001RbkH3bah9l03PH', 'ses_f78f34fafffeIugr4PB2oXkVD0', 'assistant', 5, 1788972153713, 1788972154157, '{"time":{"created":1788972153713,"streamed":1788972154151,"completed":1788972154156},"agent":"build","model":{"id":"big-pickle","providerID":"opencode","variant":"default"},"content":[{"type":"reasoning","text":"The user wants me to read input.txt and reply with the three words in order and the number of lines. Let me read the file.","state":{"reasoningField":"reasoning_content"},"time":{"created":1788972153715,"completed":1788972154145}},{"type":"tool","id":"call_5d26534d4ebd4081ba759081","name":"read","executed":false,"state":{"status":"completed","input":{"path":"/workspace/project-a/input.txt"},"content":[{"type":"text","text":"Read file /workspace/project-a/input.txt, lines 1-3\n1: alpha\n2: beta\n3: gamma"}],"metadata":{"truncated":false}},"time":{"created":1788972153842,"ran":1788972154149,"completed":1788972154155}}],"finish":"tool-calls","rawFinish":"tool_calls","cost":0,"tokens":{"input":6900,"output":93,"reasoning":0,"cache":{"read":0,"write":0}}}');

INSERT INTO session_message VALUES ('msg_0870cbd3e0016cohjV9lLVSSlb', 'ses_f78f34fafffeIugr4PB2oXkVD0', 'assistant', 14, 1788972155997, 1788972156291, '{"time":{"created":1788972155997,"streamed":1788972156289,"completed":1788972156290},"agent":"build","model":{"id":"big-pickle","providerID":"opencode","variant":"default"},"content":[{"type":"reasoning","text":"The file has 3 lines with the words:\n1. alpha\n2. beta\n3. gamma","state":{"reasoningField":"reasoning_content"},"time":{"created":1788972156000,"completed":1788972156285}},{"type":"text","text":"Here are the results:\n\n**Three words in order:**\n1. alpha\n2. beta\n3. gamma\n\n**Number of lines:** 3"}],"finish":"stop","rawFinish":"stop","cost":0,"tokens":{"input":223,"output":55,"reasoning":0,"cache":{"read":6848,"write":0}}}');

INSERT INTO session_message VALUES ('msg_0870cc7f50017RrFZvKcQsqkyW', 'ses_f78f34fafffeIugr4PB2oXkVD0', 'user', 24, 1788972157036, 1788972157037, '{"time":{"created":1788972157036},"text":"\"What was the second word in the file you just read? Answer with that word only, without tools.\"","files":[]}');

INSERT INTO session_message VALUES ('msg_0870cc87c001se0b6mpqTc3W7f', 'ses_f78f34fafffeIugr4PB2oXkVD0', 'assistant', 25, 1788972159113, 1788972159325, '{"time":{"created":1788972159113,"streamed":1788972159323,"completed":1788972159324},"agent":"build","model":{"id":"big-pickle","providerID":"opencode","variant":"default"},"content":[{"type":"reasoning","text":"The user is asking me to recall the second word from the file I just read, without using any tools.","state":{"reasoningField":"reasoning_content"},"time":{"created":1788972159116,"completed":1788972159319}},{"type":"text","text":"beta"}],"finish":"stop","rawFinish":"stop","cost":0,"tokens":{"input":114,"output":26,"reasoning":0,"cache":{"read":7040,"write":0}}}');

INSERT INTO session_message VALUES ('msg_087118959001Ui423lAgRn1xqP', 'ses_f78ee770effebwbWXea2fzwUIu', 'user', 4, 1788972468607, 1788972468607, '{"time":{"created":1788972468607},"text":"\"Use the bash tool to run exactly `printf \\\"beta failure check\\n\\\"; exit 7`. Report the exit code. Do not edit files or run any other command.\"","files":[]}');

INSERT INTO session_message VALUES ('msg_087118987001yHxUpvv8eekMEu', 'ses_f78ee770effebwbWXea2fzwUIu', 'assistant', 5, 1788972471881, 1788972472123, '{"time":{"created":1788972471881,"streamed":1788972472097,"completed":1788972472122},"agent":"build","model":{"id":"big-pickle","providerID":"opencode","variant":"default"},"content":[{"type":"reasoning","text":"The user wants me to run a specific bash command and report the exit code.","state":{"reasoningField":"reasoning_content"},"time":{"created":1788972471883,"completed":1788972472093}},{"type":"tool","id":"call_58f0f85c4ab04876a3c8f994","name":"shell","executed":false,"state":{"status":"completed","input":{"command":"printf \"beta failure check\\n\"; exit 7"},"content":[{"type":"text","text":"beta failure check\n"},{"type":"text","text":"Command exited with code 7."}],"metadata":{"status":"completed","truncated":false,"exit":7}},"time":{"created":1788972472001,"ran":1788972472095,"completed":1788972472121}}],"finish":"tool-calls","rawFinish":"tool_calls","cost":0,"tokens":{"input":6902,"output":48,"reasoning":0,"cache":{"read":0,"write":0}}}');

INSERT INTO session_message VALUES ('msg_087119744001kSjY6DiCv97aCe', 'ses_f78ee770effebwbWXea2fzwUIu', 'assistant', 14, 1788972475451, 1788972476189, '{"time":{"created":1788972475451,"streamed":1788972476188,"completed":1788972476189},"agent":"build","model":{"id":"big-pickle","providerID":"opencode","variant":"default"},"content":[{"type":"reasoning","text":"The command ran successfully (in terms of executing), printed \"beta failure check\", and exited with code 7.","state":{"reasoningField":"reasoning_content"},"time":{"created":1788972475452,"completed":1788972476186}},{"type":"text","text":"The command printed:\n\n```\nbeta failure check\n```\n\nand exited with **exit code 7**."}],"finish":"stop","rawFinish":"stop","cost":0,"tokens":{"input":124,"output":48,"reasoning":0,"cache":{"read":6848,"write":0}}}');

CREATE UNIQUE INDEX `session_message_session_seq_idx` ON `session_message` (`session_id`,`seq`);

CREATE INDEX `session_message_session_type_seq_idx` ON `session_message` (`session_id`,`type`,`seq`);

CREATE INDEX `session_message_session_time_created_id_idx` ON `session_message` (`session_id`,`time_created`,`id`);

CREATE INDEX `session_message_time_created_idx` ON `session_message` (`time_created`);
