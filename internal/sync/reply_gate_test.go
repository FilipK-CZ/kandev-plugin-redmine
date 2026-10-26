package sync

import (
	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
	"kandev-plugin-redmine/internal/issues"
	"kandev-plugin-redmine/internal/tasklink"
	"testing"
)

func TestInboundSyncCannotMoveTaskOutOfConfiguredHumanReplyGate(t *testing.T) {
	for _, tc := range []struct{ name, step, gate, want string }{
		{"human approval", "reply-step", "reply-step", ""},
		{"other step retains normal mapping", "step-backlog", "reply-step", "step-done"},
		{"drafting disabled retains normal mapping", "reply-step", "", "step-done"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := buildInboundPlan("task", pluginsdk.Task{WorkflowStepID: tc.step, Title: "Old title"}, tasklink.Link{}, issues.Issue{StatusID: 2, Subject: "Current title"}, testMapping(), Options{SyncTitleDescription: true, PreserveWorkflowStepIDs: []string{tc.gate}})
			require.Equal(t, tc.want, plan.workflowStepID)
			require.NotNil(t, plan.update.Title, "human gate must not disable ordinary field synchronization")
		})
	}
}

func TestRoutedWorkflowAndBothCommunicationGatesKeepTheirPlacement(t *testing.T) {
	for _, tc := range []struct{ workflow, step string }{
		{"bug-flow", "diagnosis"}, {"bug-flow", "first-reply"}, {"bug-flow", "final-reply"},
		{"", "first-reply"}, {"", "final-reply"},
	} {
		plan := buildInboundPlan("task", pluginsdk.Task{WorkflowID: tc.workflow, WorkflowStepID: tc.step, Title: "Old"}, tasklink.Link{}, issues.Issue{StatusID: 2, Subject: "New"}, testMapping(), Options{SyncTitleDescription: true, PreserveWorkflowStepIDs: []string{"first-reply", "final-reply"}})
		require.Empty(t, plan.workflowStepID)
		require.NotNil(t, plan.update.Title, "routing must not disable field synchronization")
	}
}
