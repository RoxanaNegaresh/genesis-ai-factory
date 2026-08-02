package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// ArtifactKind is the typed output of an agent. Agents exchange artifacts, not
// prose: this is what makes the pipeline verifiable.
type ArtifactKind string

const (
	ArtifactVision       ArtifactKind = "product.vision"
	ArtifactPRD          ArtifactKind = "product.prd"
	ArtifactDesignSystem ArtifactKind = "design.system"
	ArtifactDesignFlows  ArtifactKind = "design.flows"
	ArtifactArchSpec     ArtifactKind = "arch.spec"
	ArtifactADR          ArtifactKind = "arch.adr"
	ArtifactDBSchema     ArtifactKind = "db.schema"
	ArtifactMigrations   ArtifactKind = "db.migrations"
	ArtifactTaskPlan     ArtifactKind = "plan.tasks"
	ArtifactCodeBackend  ArtifactKind = "code.backend"
	ArtifactCodeFrontend ArtifactKind = "code.frontend"
	ArtifactQAReport     ArtifactKind = "qa.report"
	ArtifactSecReport    ArtifactKind = "sec.report"
	ArtifactDocker       ArtifactKind = "ops.docker"
	ArtifactCI           ArtifactKind = "ops.ci"
	ArtifactReadme       ArtifactKind = "docs.readme"
	ArtifactImprovePlan  ArtifactKind = "improve.plan"
)

// Storage selects where the artifact bytes live. Small structured documents go
// inline in the database; large blobs go to the filesystem or object storage.
type Storage string

const (
	StorageInline Storage = "db"
	StorageFile   Storage = "fs"
	StorageObject Storage = "s3"
)

// InlineLimit is the size above which an artifact is written to disk instead of
// the database row.
const InlineLimit = 256 * 1024

// Artifact is a content-addressed output of the factory.
type Artifact struct {
	ID        ID
	RunID     ID
	TaskID    *ID
	ProjectID ID
	Kind      ArtifactKind
	Name      string
	MIME      string
	SizeBytes int64
	SHA256    string
	Storage   Storage
	Body      string
	Path      string
	Metadata  Settings
	CreatedAt time.Time
}

// NewArtifact builds an artifact from a body, computing its content hash so
// identical regenerations deduplicate instead of accumulating.
func NewArtifact(projectID, runID ID, taskID *ID, kind ArtifactKind, name, mime, body string, now time.Time) *Artifact {
	sum := sha256.Sum256([]byte(body))
	a := &Artifact{
		ID:        NewID(),
		RunID:     runID,
		TaskID:    taskID,
		ProjectID: projectID,
		Kind:      kind,
		Name:      name,
		MIME:      mime,
		SizeBytes: int64(len(body)),
		SHA256:    hex.EncodeToString(sum[:]),
		Storage:   StorageInline,
		Body:      body,
		Metadata:  Settings{},
		CreatedAt: now.UTC(),
	}
	if a.SizeBytes > InlineLimit {
		a.Storage = StorageFile
	}
	return a
}

// HashContent returns the content hash used for addressing and for verifying
// that a file has not changed since it was read.
//
// This lives in the domain because it defines identity for content across the
// whole system — the patch engine, the editor and artifact deduplication must
// all agree on what "the same content" means.
func HashContent(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// Workspace is the on-disk repository backing a project.
type Workspace struct {
	ID             ID
	ProjectID      ID
	RootPath       string
	VCS            string
	DefaultBranch  string
	CurrentBranch  string
	HeadCommit     string
	Status         string
	LastSnapshotAt *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// WorkspaceFile is the metadata index over a workspace, used for fast tree
// rendering and search in the desktop app. The bytes remain on disk; git is the
// source of truth.
type WorkspaceFile struct {
	ID               ID
	WorkspaceID      ID
	RelPath          string
	Lang             string
	SizeBytes        int64
	SHA256           string
	GeneratedByAgent AgentRole
	IsUserModified   bool
	LastModifiedAt   time.Time
	CreatedAt        time.Time
}
