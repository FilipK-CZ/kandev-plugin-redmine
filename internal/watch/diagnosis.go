package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"kandev-plugin-redmine/internal/issues"
)

const diagnosticClaimKey = "diagnostic_claim"

type diagnosticClaim struct {
	State           string `json:"state"`
	IssueID         int    `json:"issue_id"`
	ProjectID       int    `json:"project_id"`
	TrackerID       int    `json:"tracker_id"`
	TokenUserID     int    `json:"token_user_id"`
	WorkflowID      string `json:"workflow_id"`
	StepID          string `json:"step_id"`
	ProfileID       string `json:"profile_id"`
	FromStatusID    int    `json:"from_status_id"`
	WorkingStatusID int    `json:"working_status_id"`
}

func (s *Service) saveDiagnosticClaim(ctx context.Context, taskID string, claim diagnosticClaim) error {
	encoded, err := json.Marshal(claim)
	if err != nil {
		return err
	}
	var value map[string]any
	if err := json.Unmarshal(encoded, &value); err != nil {
		return err
	}
	return s.host.SetState(ctx, "task", taskID, diagnosticClaimKey, value)
}

// A created task is not proof of launch: StartAgent is best-effort in Kandev.
func (s *Service) claimDiagnosis(ctx context.Context, w Watch, taskID string, tokenUserID int, issuesSvc *issues.Service) error {
	value, found, err := s.host.GetState(ctx, "task", taskID, diagnosticClaimKey)
	if err != nil || !found {
		return err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var claim diagnosticClaim
	if err := json.Unmarshal(encoded, &claim); err != nil {
		return err
	}
	if claim.State != "pending" && claim.State != "writing" {
		return nil
	}
	finish := func(state string) error {
		claim.State = state
		return s.saveDiagnosticClaim(ctx, taskID, claim)
	}
	link, linked, err := s.tasklinks.Get(ctx, taskID)
	if err != nil {
		return err
	}
	if !linked || link.WorkspaceID != w.WorkspaceID || link.IssueID != claim.IssueID {
		return fmt.Errorf("watch: diagnostic claim for task %s lost its issue link; inspect before continuing", taskID)
	}
	task, err := s.host.Tasks().Get(ctx, taskID)
	if err != nil {
		return err
	}
	if task == nil || task.WorkspaceID != w.WorkspaceID || task.WorkflowID != claim.WorkflowID || taskPluginMetadata(task)[metadataKeyWatchID] != w.ID {
		return fmt.Errorf("watch: diagnostic claim task %s is outside its original watch/workflow", taskID)
	}
	if claim.State == "pending" {
		if task.State == "FAILED" || task.State == "CANCELLED" {
			return fmt.Errorf("watch: diagnostic task %s failed before its claim was confirmed; inspect the native launch", taskID)
		}
		configured := false
		for _, route := range w.TrackerRoutes {
			if route.RedmineTrackerID == claim.TrackerID && route.WorkflowID == claim.WorkflowID && route.WorkflowStepID == claim.StepID && route.Diagnosis != nil && route.Diagnosis.FromStatusID == claim.FromStatusID && route.Diagnosis.WorkingStatusID == claim.WorkingStatusID {
				entry, err := route.DiagnosticEntry(w.EntrySteps[route.WorkflowID])
				if err != nil {
					return err
				}
				configured = entry.AgentProfileID == claim.ProfileID && tokenUserID == claim.TokenUserID
			}
		}
		if !configured {
			return finish("cancelled")
		}
		started, err := s.diagnosisStarted(ctx, w.WorkspaceID, taskID, claim.ProfileID)
		if err != nil || !started {
			return err
		}
	}
	current, err := issuesSvc.GetIssue(ctx, claim.IssueID)
	if err != nil {
		return err
	}
	sameOwner := current.ProjectID == claim.ProjectID && current.TrackerID == claim.TrackerID && current.AssigneeID == claim.TokenUserID && tokenUserID == claim.TokenUserID
	if claim.State == "pending" {
		if !sameOwner || current.StatusID != claim.FromStatusID {
			return finish("skipped_changed_issue")
		}
		claim.State = "writing"
		if err := s.saveDiagnosticClaim(ctx, taskID, claim); err != nil {
			return err
		}
		// shortcut: Redmine REST exposes no lock version, so GET/PUT is not atomic;
		// use a server-side conditional endpoint if strict concurrent claims are needed.
		if err := issuesSvc.SetStatusOnce(ctx, claim.IssueID, claim.WorkingStatusID); err != nil {
			return fmt.Errorf("watch: diagnostic status write for task %s has an unconfirmed result; no automatic retry: %w", taskID, err)
		}
		current, err = issuesSvc.GetIssue(ctx, claim.IssueID)
		if err != nil {
			return err
		}
		sameOwner = current.ProjectID == claim.ProjectID && current.TrackerID == claim.TrackerID && current.AssigneeID == claim.TokenUserID
	}
	if !sameOwner || current.StatusID != claim.WorkingStatusID {
		return fmt.Errorf("watch: diagnostic status write for task %s requires inspection; it will not be repeated", taskID)
	}
	if err := s.tasklinks.RecordPushedStatus(ctx, taskID, claim.WorkingStatusID); err != nil {
		return err
	}
	if err := s.tasklinks.ObserveStatus(ctx, taskID, claim.WorkingStatusID, claim.TrackerID, false); err != nil {
		return err
	}
	return finish("complete")
}

func (s *Service) diagnosisStarted(ctx context.Context, workspaceID, taskID, profileID string) (bool, error) {
	sessions, page, err := s.host.Sessions().List(ctx, pluginsdk.SessionFilter{TaskIDs: []string{taskID}, WorkspaceIDs: []string{workspaceID}}, pluginsdk.Page{Limit: 100})
	if err != nil {
		return false, err
	}
	if page != nil && page.HasMore {
		return false, fmt.Errorf("watch: diagnostic session history needs inspection")
	}
	var first *pluginsdk.Session
	var earliest time.Time
	for i := range sessions {
		session := &sessions[i]
		started, err := time.Parse(time.RFC3339Nano, session.StartedAt)
		if session.TaskID != taskID || err != nil {
			return false, fmt.Errorf("watch: invalid diagnostic session scope or timestamp")
		}
		if first == nil || started.Before(earliest) {
			first, earliest = session, started
		}
	}
	if first == nil {
		return false, nil
	}
	if first.AgentProfileID != profileID {
		return false, fmt.Errorf("watch: diagnostic task started with a different profile")
	}
	switch first.State {
	case "CREATED", "STARTING":
		return false, nil
	case "RUNNING", "WAITING_FOR_INPUT", "IDLE", "COMPLETED":
		return first.ACPSessionID != "", nil
	default:
		return false, fmt.Errorf("watch: diagnostic session %s did not start successfully (%s)", first.ID, first.State)
	}
}
