// Package issues owns Redmine issue read/write and the two-step attachment
// upload-token flow, layered on redmineclient's generic authenticated verbs.
package issues

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strconv"

	"kandev-plugin-redmine/internal/redmineclient"
)

// Issue is the plugin's normalized view of a Redmine issue, flattened from
// Redmine's nested {id,name} reference shape (Project/Tracker/Status/
// Priority) to plain IDs.
type Issue struct {
	ID           int
	URL          string
	Subject      string
	Description  string
	ProjectID    int
	TrackerID    int
	StatusID     int
	PriorityID   int
	AssigneeID   int
	CategoryID   int
	UpdatedOn    string
	CustomFields []CustomFieldValue
	Journals     []Journal
	Attachments  []Attachment
	Relations    []Relation
}

type CustomFieldValue struct {
	ID    int    `json:"id"`
	Name  string `json:"name,omitempty"`
	Value any    `json:"value"`
}

type Journal struct {
	ID           int    `json:"id"`
	Notes        string `json:"notes"`
	CreatedOn    string `json:"created_on"`
	PrivateNotes bool   `json:"private_notes"`
	User         struct {
		ID int `json:"id"`
	} `json:"user"`
}

type Attachment struct {
	ID       int    `json:"id"`
	Filename string `json:"filename"`
}

type Relation struct {
	ID           int    `json:"id"`
	IssueToID    int    `json:"issue_to_id"`
	RelationType string `json:"relation_type"`
}

type idRef struct {
	ID int `json:"id"`
}

type rawIssue struct {
	ID           int                `json:"id"`
	Subject      string             `json:"subject"`
	Description  string             `json:"description"`
	Project      idRef              `json:"project"`
	Tracker      idRef              `json:"tracker"`
	Status       idRef              `json:"status"`
	Priority     idRef              `json:"priority"`
	Assignee     idRef              `json:"assigned_to"`
	Category     idRef              `json:"category"`
	UpdatedOn    string             `json:"updated_on"`
	CustomFields []CustomFieldValue `json:"custom_fields"`
	Journals     []Journal          `json:"journals"`
	Attachments  []Attachment       `json:"attachments"`
	Relations    []Relation         `json:"relations"`
}

func (r rawIssue) toIssue(baseURL string) Issue {
	return Issue{
		ID:           r.ID,
		URL:          fmt.Sprintf("%s/issues/%d", baseURL, r.ID),
		Subject:      r.Subject,
		Description:  r.Description,
		ProjectID:    r.Project.ID,
		TrackerID:    r.Tracker.ID,
		StatusID:     r.Status.ID,
		PriorityID:   r.Priority.ID,
		AssigneeID:   r.Assignee.ID,
		CategoryID:   r.Category.ID,
		UpdatedOn:    r.UpdatedOn,
		CustomFields: r.CustomFields,
		Journals:     r.Journals,
		Attachments:  r.Attachments,
		Relations:    r.Relations,
	}
}

type issueEnvelope struct {
	Issue rawIssue `json:"issue"`
}

// Service performs issue reads/writes against one connected Redmine
// instance.
type Service struct {
	client *redmineclient.Client
}

func New(client *redmineclient.Client) *Service {
	return &Service{client: client}
}

func (s *Service) TokenUserID(ctx context.Context) (int, error) {
	user, err := s.client.ValidateCredentials(ctx)
	if err != nil {
		return 0, err
	}
	if user.ID <= 0 {
		return 0, fmt.Errorf("redmine: token owner is unavailable")
	}
	return user.ID, nil
}

// SetStatusOnce leaves uncertain results for read-only reconciliation.
func (s *Service) SetStatusOnce(ctx context.Context, id, statusID int) error {
	return s.client.PutJSONOnce(ctx, fmt.Sprintf("/issues/%d.json", id), map[string]any{
		"issue": map[string]any{"status_id": statusID},
	}, nil)
}

// GetIssue fetches full issue detail (journals/attachments/relations
// included) for read-only display.
func (s *Service) GetIssue(ctx context.Context, id int) (*Issue, error) {
	var out issueEnvelope
	if err := s.client.GetJSON(ctx, fmt.Sprintf("/issues/%d.json", id),
		map[string]string{"include": "journals,attachments,relations"}, &out); err != nil {
		return nil, err
	}
	issue := out.Issue.toIssue(s.client.BaseURL())
	return &issue, nil
}

// AddComment changes only the journal; field updates and their safe retries
// continue through UpdateIssueFields.
func (s *Service) AddComment(ctx context.Context, id int, notes string, private bool) error {
	return s.client.PutJSONOnce(ctx, fmt.Sprintf("/issues/%d.json", id), map[string]any{
		"issue": map[string]any{"notes": notes, "private_notes": private},
	}, nil)
}

// ListIssuesParams filters one page of /issues.json.
type ListIssuesParams struct {
	ProjectID string
	// UpdatedOnFrom is a Redmine date-filter operator string, e.g.
	// ">=2026-01-01T00:00:00Z" (see internal/sync's cursor-based poll).
	UpdatedOnFrom string
	// Filters contains Redmine API filter query parameters. Callers only
	// populate validated, plugin-owned keys (tracker_id, status_id,
	// priority_id, assigned_to_id, category_id, and cf_<positive-id>).
	Filters map[string]string
	// NativeFilters uses Redmine's advanced issue-filter grammar, so callers
	// can offer the same add/remove field model as Redmine's Issues page.
	NativeFilters []NativeFilter
	Offset        int
	Limit         int
}

// NativeFilter is one Redmine issue-list predicate. Field and operator are
// server-validated before this package receives them.
type NativeFilter struct {
	Field    string
	Operator string
	Value    string
}

type ListIssuesResult struct {
	Issues     []Issue
	TotalCount int
}

type issuesListEnvelope struct {
	Issues     []rawIssue `json:"issues"`
	TotalCount int        `json:"total_count"`
}

// ListIssues fetches one page of issues, always sending status_id=* —
// Redmine defaults to open-only, which would silently drop closed-issue
// updates from sync and watcher polling (spec "Issue read/write").
func (s *Service) ListIssues(ctx context.Context, params ListIssuesParams) (*ListIssuesResult, error) {
	query := url.Values{"status_id": {"*"}}
	if params.ProjectID != "" {
		query.Set("project_id", params.ProjectID)
	}
	if params.UpdatedOnFrom != "" {
		query.Set("updated_on", params.UpdatedOnFrom)
	}
	for key, value := range params.Filters {
		if value != "" {
			query.Set(key, value)
		}
	}
	limit := params.Limit
	if limit <= 0 {
		limit = 100
	}
	for _, filter := range params.NativeFilters {
		if filter.Field == "" || filter.Operator == "" || filter.Value == "" {
			continue
		}
		query.Set("set_filter", "1")
		query.Add("f[]", filter.Field)
		query.Set("op["+filter.Field+"]", filter.Operator)
		query.Add("v["+filter.Field+"][]", filter.Value)
	}
	query.Set("offset", strconv.Itoa(params.Offset))
	query.Set("limit", strconv.Itoa(limit))

	var out issuesListEnvelope
	if err := s.client.GetJSONValues(ctx, "/issues.json", query, &out); err != nil {
		return nil, err
	}
	result := ListIssuesResult{Issues: make([]Issue, len(out.Issues)), TotalCount: out.TotalCount}
	for i, raw := range out.Issues {
		result.Issues[i] = raw.toIssue(s.client.BaseURL())
	}
	return &result, nil
}

// IssueWrite is the field set CreateIssue/UpdateIssue accept.
type IssueWrite struct {
	ProjectID    int
	TrackerID    int
	StatusID     int
	PriorityID   int
	Subject      string
	Description  string
	CustomFields []CustomFieldValue
	// Uploads are the token and file metadata returned/provided by Redmine's
	// two-step upload flow. Redmine requires filename on both the upload query
	// and the issue write payload.
	Uploads []Upload
}

// IssueUpdate preserves JSON-field presence for partial issue updates. A nil
// pointer means leave the Redmine value untouched; a non-nil pointer may
// deliberately carry an empty string (for example to clear description).
type IssueUpdate struct {
	ProjectID    *int
	TrackerID    *int
	StatusID     *int
	PriorityID   *int
	Subject      *string
	Description  *string
	CustomFields *[]CustomFieldValue
	Uploads      *[]Upload
}

type uploadRef struct {
	Token       string `json:"token"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type,omitempty"`
}

type Upload struct {
	Token       string `json:"token"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type,omitempty"`
}

type issueWriteBody struct {
	ProjectID    int                `json:"project_id,omitempty"`
	TrackerID    int                `json:"tracker_id,omitempty"`
	StatusID     int                `json:"status_id,omitempty"`
	PriorityID   int                `json:"priority_id,omitempty"`
	Subject      string             `json:"subject,omitempty"`
	Description  string             `json:"description,omitempty"`
	CustomFields []CustomFieldValue `json:"custom_fields,omitempty"`
	Uploads      []uploadRef        `json:"uploads,omitempty"`
}

type issueWritePayload struct {
	Issue issueWriteBody `json:"issue"`
}

type issueUpdateBody struct {
	ProjectID    *int                `json:"project_id,omitempty"`
	TrackerID    *int                `json:"tracker_id,omitempty"`
	StatusID     *int                `json:"status_id,omitempty"`
	PriorityID   *int                `json:"priority_id,omitempty"`
	Subject      *string             `json:"subject,omitempty"`
	Description  *string             `json:"description,omitempty"`
	CustomFields *[]CustomFieldValue `json:"custom_fields,omitempty"`
	Uploads      *[]uploadRef        `json:"uploads,omitempty"`
}

type issueUpdatePayload struct {
	Issue issueUpdateBody `json:"issue"`
}

func (w IssueWrite) toBody() issueWriteBody {
	uploads := make([]uploadRef, len(w.Uploads))
	for i, upload := range w.Uploads {
		uploads[i] = uploadRef{Token: upload.Token, Filename: upload.Filename, ContentType: upload.ContentType}
	}
	return issueWriteBody{
		ProjectID:    w.ProjectID,
		TrackerID:    w.TrackerID,
		StatusID:     w.StatusID,
		PriorityID:   w.PriorityID,
		Subject:      w.Subject,
		Description:  w.Description,
		CustomFields: w.CustomFields,
		Uploads:      uploads,
	}
}

func (w IssueUpdate) toBody() issueUpdateBody {
	body := issueUpdateBody{
		ProjectID: w.ProjectID, TrackerID: w.TrackerID, StatusID: w.StatusID,
		PriorityID: w.PriorityID, Subject: w.Subject, Description: w.Description,
		CustomFields: w.CustomFields,
	}
	if w.Uploads != nil {
		uploads := make([]uploadRef, len(*w.Uploads))
		for i, upload := range *w.Uploads {
			uploads[i] = uploadRef{Token: upload.Token, Filename: upload.Filename, ContentType: upload.ContentType}
		}
		body.Uploads = &uploads
	}
	return body
}

// CreateIssue creates a new issue via POST /issues.json, returning its id
// and browsable URL.
func (s *Service) CreateIssue(ctx context.Context, write IssueWrite) (*Issue, error) {
	var out issueEnvelope
	if err := s.client.PostJSON(ctx, "/issues.json", issueWritePayload{Issue: write.toBody()}, &out); err != nil {
		return nil, err
	}
	issue := out.Issue.toIssue(s.client.BaseURL())
	return &issue, nil
}

// UpdateIssue updates an existing issue via PUT /issues/:id.json. Redmine
// answers a successful PUT with an empty body, so the response is not
// decoded.
func (s *Service) UpdateIssue(ctx context.Context, id int, write IssueWrite) error {
	return s.client.PutJSON(ctx, fmt.Sprintf("/issues/%d.json", id), issueWritePayload{Issue: write.toBody()}, nil)
}

// UpdateIssueFields performs a partial update while retaining the caller's
// presence information. In particular, Description: ptr("") serializes an
// explicit clear whereas Description: nil omits description entirely.
func (s *Service) UpdateIssueFields(ctx context.Context, id int, update IssueUpdate) error {
	return s.client.PutJSON(ctx, fmt.Sprintf("/issues/%d.json", id), issueUpdatePayload{Issue: update.toBody()}, nil)
}

type uploadEnvelope struct {
	Upload struct {
		Token string `json:"token"`
	} `json:"upload"`
}

// UploadAttachment performs the two-step attachment flow's first step: POST
// /uploads.json?filename=<name> with the raw file bytes and Content-Type:
// application/octet-stream, returning the token to pass as one of
// IssueWrite.Uploads on a following CreateIssue/UpdateIssue call.
func (s *Service) UploadAttachment(ctx context.Context, filename, contentType string, content io.Reader) (Upload, error) {
	if filename == "" {
		return Upload{}, fmt.Errorf("issues: attachment filename is required")
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	var out uploadEnvelope
	// Redmine's uploads endpoint accepts raw binary with this fixed transport
	// media type. The user-provided MIME belongs to the later uploads[] issue
	// metadata, not to this HTTP request.
	if err := s.client.PostBinary(ctx, "/uploads.json", "application/octet-stream", content, map[string]string{"filename": filename}, &out); err != nil {
		return Upload{}, err
	}
	return Upload{Token: out.Upload.Token, Filename: filename, ContentType: contentType}, nil
}
