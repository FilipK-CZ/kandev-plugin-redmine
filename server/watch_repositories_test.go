package main

import (
	"context"
	"testing"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
	"kandev-plugin-redmine/internal/watch"
)

type repositoryChoices struct {
	rows         []pluginsdk.Repository
	repeatCursor bool
}

func (r repositoryChoices) List(context.Context, string, pluginsdk.Page) ([]pluginsdk.Repository, *pluginsdk.PageInfo, error) {
	if r.repeatCursor {
		return r.rows, &pluginsdk.PageInfo{HasMore: true, NextCursor: "again"}, nil
	}
	return r.rows, nil, nil
}

func TestWatchRepositoryChoicesAreScopedAndDoNotExposeCredentialsOrURLs(t *testing.T) {
	p, host := newTestPlugin(t)
	branch := "develop"
	host.repositoryReader = repositoryChoices{rows: []pluginsdk.Repository{{ID: "repo", WorkspaceID: "ws", Name: "project/application", DefaultBranch: &branch, RemoteURL: "https://redacted.example/source.git"}}}
	response := handle(t, p, "watches.repositories", "ws", "", nil)
	rows := response["repositories"].([]any)
	require.Len(t, rows, 1)
	require.Equal(t, map[string]any{"id": "repo", "name": "project/application", "default_branch": "develop"}, rows[0])
	require.NoError(t, p.validateWatchRepository(context.Background(), watch.Watch{WorkspaceID: "ws", RepositoryID: "repo"}))
	require.Error(t, p.validateWatchRepository(context.Background(), watch.Watch{WorkspaceID: "ws", RepositoryID: "foreign"}))
	host.repositoryReader = repositoryChoices{rows: []pluginsdk.Repository{{ID: "foreign", WorkspaceID: "another-workspace"}}}
	_, err := p.watchRepositories(context.Background(), "ws")
	require.ErrorContains(t, err, "outside requested workspace")
}

func TestWatchRepositoryPaginationDoesNotLoop(t *testing.T) {
	p, host := newTestPlugin(t)
	host.repositoryReader = repositoryChoices{repeatCursor: true}
	_, err := p.watchRepositories(context.Background(), "ws")
	require.ErrorContains(t, err, "invalid repository pagination")
}
