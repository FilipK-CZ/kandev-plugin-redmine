package main

import (
	"context"
	"fmt"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"kandev-plugin-redmine/internal/watch"
)

type watchRepositoryOption struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch,omitempty"`
}

func (p *redminePlugin) watchRepositories(ctx context.Context, workspaceID string) ([]watchRepositoryOption, error) {
	var result []watchRepositoryOption
	page := pluginsdk.Page{Limit: 100}
	seen := map[string]bool{page.Cursor: true}
	for {
		items, info, err := p.Host().Repositories().List(ctx, workspaceID, page)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if item.WorkspaceID != workspaceID {
				return nil, fmt.Errorf("redmine: repository outside requested workspace")
			}
			option := watchRepositoryOption{ID: item.ID, Name: item.Name}
			if item.DefaultBranch != nil {
				option.DefaultBranch = *item.DefaultBranch
			}
			result = append(result, option)
		}
		if info == nil || !info.HasMore {
			return result, nil
		}
		if info.NextCursor == "" || seen[info.NextCursor] {
			return nil, fmt.Errorf("redmine: invalid repository pagination")
		}
		page.Cursor = info.NextCursor
		seen[page.Cursor] = true
	}
}

func (p *redminePlugin) handleWatchRepositories(ctx context.Context, req *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	items, err := p.watchRepositories(ctx, req.Context.WorkspaceID)
	if err != nil {
		return classifiedErrorResponse(err)
	}
	return jsonResponse(map[string]any{"repositories": items})
}

func (p *redminePlugin) validateWatchRepository(ctx context.Context, w watch.Watch) error {
	items, err := p.watchRepositories(ctx, w.WorkspaceID)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.ID == w.RepositoryID {
			return nil
		}
	}
	return fmt.Errorf("redmine: repository is unavailable in this workspace")
}
