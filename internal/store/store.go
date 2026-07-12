package store

import (
	"context"
	"errors"

	"github.com/swiftdiaries/agent-transcripts/internal/session"
)

var (
	ErrConflict  = errors.New("store conflict")
	ErrNotFound  = errors.New("session not found")
	ErrForbidden = errors.New("uploader does not own session")
)

type Store interface {
	ListDirectories(context.Context, string) ([]session.Directory, error)
	CreateProject(context.Context, string) error
	ListSessions(context.Context, session.Directory) ([]session.Metadata, error)
	GetSession(context.Context, string) (session.Package, error)
	PutSession(context.Context, session.Package) (bool, error)
	UpdateMetadata(context.Context, string, string, session.Metadata) (string, error)
	MoveSession(context.Context, string, string, session.Directory) (session.Metadata, error)
	DeleteSession(context.Context, string, string) error
}
