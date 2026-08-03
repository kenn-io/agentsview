package bunmodel

import "github.com/uptrace/bun"

type SourceArchive struct {
	bun.BaseModel `bun:"table:source_archives"`

	SourceArchiveID   string `bun:"source_archive_id,pk"`
	SourceArchiveSalt string `bun:"source_archive_salt,notnull"`
}

type SourceProjectIdentityObservation struct {
	bun.BaseModel `bun:"table:source_project_identity_observations"`

	SourceArchiveID      string    `bun:"source_archive_id,pk"`
	SourceArchiveSalt    string    `bun:"source_archive_salt,notnull"`
	Project              string    `bun:"project,pk"`
	Machine              string    `bun:"machine,pk"`
	RootPath             string    `bun:"root_path,pk"`
	GitRemote            string    `bun:"git_remote,pk"`
	GitRemoteName        string    `bun:"git_remote_name,notnull,default:''"`
	RepositoryPath       string    `bun:"repository_path,notnull,default:''"`
	WorktreeName         string    `bun:"worktree_name,notnull,default:''"`
	WorktreeRootPath     string    `bun:"worktree_root_path,notnull,default:''"`
	WorktreeRelationship string    `bun:"worktree_relationship,notnull,default:'unknown'"`
	CheckoutState        string    `bun:"checkout_state,notnull,default:'unknown'"`
	GitBranch            string    `bun:"git_branch,notnull,default:''"`
	RemoteResolution     string    `bun:"remote_resolution,notnull,default:'unknown'"`
	RemoteCandidateCount int       `bun:"remote_candidate_count,notnull,default:0"`
	ObservedAt           Timestamp `bun:"observed_at,type:TIMESTAMPTZ,notnull"`
	NormalizedRemote     string    `bun:"normalized_remote,notnull,default:''"`
	KeySource            string    `bun:"key_source,notnull,default:''"`
	Key                  string    `bun:"key,notnull,default:''"`
}

type SourceSessionProjectIdentitySnapshot struct {
	bun.BaseModel `bun:"table:source_session_project_identity_snapshots"`

	SourceArchiveID          string    `bun:"source_archive_id,pk"`
	SourceDatabaseGeneration string    `bun:"source_database_generation,pk"`
	SourceSessionID          string    `bun:"source_session_id,pk"`
	Project                  string    `bun:"project,notnull"`
	Machine                  string    `bun:"machine,notnull"`
	RootPath                 string    `bun:"root_path,notnull,default:''"`
	GitRemote                string    `bun:"git_remote,notnull,default:''"`
	GitRemoteName            string    `bun:"git_remote_name,notnull,default:''"`
	RepositoryPath           string    `bun:"repository_path,notnull,default:''"`
	WorktreeName             string    `bun:"worktree_name,notnull,default:''"`
	WorktreeRootPath         string    `bun:"worktree_root_path,notnull,default:''"`
	WorktreeRelationship     string    `bun:"worktree_relationship,notnull,default:'unknown'"`
	CheckoutState            string    `bun:"checkout_state,notnull,default:'unknown'"`
	GitBranch                string    `bun:"git_branch,notnull,default:''"`
	RemoteResolution         string    `bun:"remote_resolution,notnull,default:'unknown'"`
	RemoteCandidateCount     int       `bun:"remote_candidate_count,notnull,default:0"`
	ObservedAt               Timestamp `bun:"observed_at,type:TIMESTAMPTZ,notnull"`
	NormalizedRemote         string    `bun:"normalized_remote,notnull,default:''"`
	KeySource                string    `bun:"key_source,notnull,default:''"`
	Key                      string    `bun:"key,notnull,default:''"`
}

type SourceWorktreeProjectMapping struct {
	bun.BaseModel `bun:"table:source_worktree_project_mappings"`

	SourceArchiveID string    `bun:"source_archive_id,pk"`
	Machine         string    `bun:"machine,pk"`
	PathPrefix      string    `bun:"path_prefix,pk"`
	Layout          string    `bun:"layout,notnull,default:'explicit'"`
	Project         string    `bun:"project,notnull,default:''"`
	OriginalProject string    `bun:"original_project,notnull,default:''"`
	Enabled         bool      `bun:"enabled,notnull,default:true"`
	UpdatedAt       Timestamp `bun:"updated_at,type:TIMESTAMPTZ,notnull"`
}
