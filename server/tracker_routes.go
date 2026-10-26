package main

import (
	"context"
	"fmt"

	"kandev-plugin-redmine/internal/fieldmapping"
	"kandev-plugin-redmine/internal/redmineclient"
)

func (p *redminePlugin) validateTrackerRoutes(ctx context.Context, workspaceID string, client *redmineclient.Client, routes []fieldmapping.TrackerRoute) error {
	if len(routes) == 0 {
		return nil
	}
	if len(routes) > 20 {
		return fmt.Errorf("redmine: at most 20 tracker routes are supported")
	}
	trackers, err := client.ListTrackers(ctx)
	if err != nil {
		return err
	}
	valid := map[int]bool{}
	for _, tracker := range trackers {
		valid[tracker.ID] = true
	}
	workflows, err := listWorkspaceWorkflows(ctx, p.Host().Workflows(), workspaceID)
	if err != nil {
		return err
	}
	seen := map[int]bool{}
	for _, route := range routes {
		if !valid[route.RedmineTrackerID] || seen[route.RedmineTrackerID] || route.WorkflowID == "" || route.WorkflowStepID == "" {
			return fmt.Errorf("redmine: choose one live tracker and an entry step for each route; duplicate trackers are not allowed")
		}
		seen[route.RedmineTrackerID] = true
		found := false
		for _, workflow := range workflows {
			if workflow.ID != route.WorkflowID {
				continue
			}
			steps, err := p.Host().Workflows().ListSteps(ctx, workflow.ID)
			if err != nil {
				return err
			}
			for _, step := range steps {
				if step.ID != route.WorkflowStepID {
					continue
				}
				found = true
			}
			validSteps := map[string]bool{}
			for _, step := range steps {
				validSteps[step.ID] = true
			}
			for _, status := range route.Statuses {
				if status.WorkflowStepID != "" && !validSteps[status.WorkflowStepID] {
					return fmt.Errorf("redmine: tracker status mapping step does not belong to its selected workflow")
				}
			}
			if route.AutomaticIntake != nil {
				if _, err := route.IntakeEntry(steps); err != nil {
					return err
				}
			}
		}
		if !found {
			return fmt.Errorf("redmine: tracker route entry step is unavailable in this workspace")
		}
	}
	return nil
}
