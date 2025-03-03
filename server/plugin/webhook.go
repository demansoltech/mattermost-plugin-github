package plugin

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // GitHub webhooks are signed using sha1 https://developer.github.com/webhooks/.
	"encoding/hex"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"github.com/google/go-github/v41/github"
	"github.com/mattermost/mattermost-server/v6/model"
	"github.com/microcosm-cc/bluemonday"
)

const (
	actionOpened    = "opened"
	actionClosed    = "closed"
	actionReopened  = "reopened"
	actionSubmitted = "submitted"
	actionLabeled   = "labeled"
	actionAssigned  = "assigned"

	actionCreated = "created"
	actionDeleted = "deleted"
	actionEdited  = "edited"
)

func verifyWebhookSignature(secret []byte, signature string, body []byte) (bool, error) {
	const signaturePrefix = "sha1="
	const signatureLength = 45

	if len(signature) != signatureLength || !strings.HasPrefix(signature, signaturePrefix) {
		return false, nil
	}

	actual := make([]byte, 20)
	_, err := hex.Decode(actual, []byte(signature[5:]))
	if err != nil {
		return false, err
	}

	sb, err := signBody(secret, body)
	if err != nil {
		return false, err
	}

	return hmac.Equal(sb, actual), nil
}

func signBody(secret, body []byte) ([]byte, error) {
	computed := hmac.New(sha1.New, secret)
	_, err := computed.Write(body)
	if err != nil {
		return nil, err
	}

	return computed.Sum(nil), nil
}

// Hack to convert from github.PushEventRepository to github.Repository
func ConvertPushEventRepositoryToRepository(pushRepo *github.PushEventRepository) *github.Repository {
	repoName := pushRepo.GetFullName()
	private := pushRepo.GetPrivate()
	return &github.Repository{
		FullName: &repoName,
		Private:  &private,
	}
}

func (p *Plugin) handleWebhook(w http.ResponseWriter, r *http.Request) {
	config := p.getConfiguration()

	signature := r.Header.Get("X-Hub-Signature")
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Bad request body", http.StatusBadRequest)
		return
	}

	event, err := github.ParseWebHook(github.WebHookType(r), body)
	if err != nil {
		p.API.LogDebug("GitHub webhook content type should be set to \"application/json\"", "error", err.Error)
		http.Error(w, "wrong mime-type. should be \"application/json\"", http.StatusBadRequest)
		return
	}

	if config.EnableWebhookEventLogging {
		bodyByte, appErr := json.Marshal(event)
		if appErr != nil {
			p.API.LogWarn("Error while Marshal Webhook Request", "err", appErr.Error())
			http.Error(w, "Error while Marshal Webhook Request", http.StatusBadRequest)
			return
		}
		p.API.LogDebug("Webhook Event Log", "event", string(bodyByte))
	}
	valid, err := verifyWebhookSignature([]byte(config.WebhookSecret), signature, body)
	if err != nil {
		p.API.LogWarn("Failed to verify webhook signature", "error", err.Error())
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	if !valid {
		http.Error(w, "Not authorized", http.StatusUnauthorized)
		return
	}

	var repo *github.Repository
	var handler func()

	switch event := event.(type) {
	case *github.PullRequestEvent:
		repo = event.GetRepo()
		if p.IsNotificationOff(*repo.FullName) {
			return
		}
		handler = func() {
			p.postPullRequestEvent(event)
			p.handlePullRequestNotification(event)
			p.handlePRDescriptionMentionNotification(event)
		}
	case *github.IssuesEvent:
		repo = event.GetRepo()
		if p.IsNotificationOff(*repo.FullName) {
			return
		}
		handler = func() {
			p.postIssueEvent(event)
			p.handleIssueNotification(event)
		}
	case *github.IssueCommentEvent:
		repo = event.GetRepo()
		if p.IsNotificationOff(*repo.FullName) {
			return
		}
		handler = func() {
			p.postIssueCommentEvent(event)
			p.handleCommentMentionNotification(event)
			p.handleCommentAuthorNotification(event)
			p.handleCommentAssigneeNotification(event)
		}
	case *github.PullRequestReviewEvent:
		repo = event.GetRepo()
		if p.IsNotificationOff(*repo.FullName) {
			return
		}
		handler = func() {
			p.postPullRequestReviewEvent(event)
			p.handlePullRequestReviewNotification(event)
		}
	case *github.PullRequestReviewCommentEvent:
		repo = event.GetRepo()
		if p.IsNotificationOff(*repo.FullName) {
			return
		}
		handler = func() {
			p.postPullRequestReviewCommentEvent(event)
		}
	case *github.PushEvent:
		repo = ConvertPushEventRepositoryToRepository(event.GetRepo())
		if p.IsNotificationOff(*repo.FullName) {
			return
		}
		handler = func() {
			p.postPushEvent(event)
		}
	case *github.CreateEvent:
		repo = event.GetRepo()
		if p.IsNotificationOff(*repo.FullName) {
			return
		}
		handler = func() {
			p.postCreateEvent(event)
		}
	case *github.DeleteEvent:
		repo = event.GetRepo()
		if p.IsNotificationOff(*repo.FullName) {
			return
		}
		handler = func() {
			p.postDeleteEvent(event)
		}
	case *github.StarEvent:
		repo = event.GetRepo()
		handler = func() {
			p.postStarEvent(event)
		}
	}

	if repo == nil || handler == nil {
		return
	}

	if repo.GetPrivate() && !config.EnablePrivateRepo {
		return
	}

	handler()
}

func (p *Plugin) permissionToRepo(userID string, ownerAndRepo string) bool {
	if userID == "" {
		return false
	}

	owner, repo := parseOwnerAndRepo(ownerAndRepo, p.getBaseURL())

	if owner == "" {
		return false
	}
	if err := p.checkOrg(owner); err != nil {
		return false
	}

	info, apiErr := p.getGitHubUserInfo(userID)
	if apiErr != nil {
		return false
	}
	ctx := context.Background()
	githubClient := p.githubConnectUser(ctx, info)

	if result, _, err := githubClient.Repositories.Get(ctx, owner, repo); result == nil || err != nil {
		if err != nil {
			p.API.LogWarn("Failed fetch repository to check permission", "error", err.Error())
		}
		return false
	}

	return true
}

func (p *Plugin) excludeConfigOrgMember(user *github.User, subscription *Subscription) bool {
	if !subscription.ExcludeOrgMembers() {
		return false
	}

	info, err := p.getGitHubUserInfo(subscription.CreatorID)
	if err != nil {
		p.API.LogWarn("Failed to exclude org member", "error", err.Message)
		return false
	}

	githubClient := p.githubConnectUser(context.Background(), info)
	organization := p.getConfiguration().GitHubOrg

	return p.isUserOrganizationMember(githubClient, user, organization)
}

func (p *Plugin) postPullRequestEvent(event *github.PullRequestEvent) {
	repo := event.GetRepo()

	subs := p.GetSubscribedChannelsForRepository(repo)
	if len(subs) == 0 {
		return
	}

	action := event.GetAction()
	if action != actionOpened && action != actionLabeled && action != actionClosed {
		return
	}

	pr := event.GetPullRequest()
	eventLabel := event.GetLabel().GetName()
	labels := make([]string, len(pr.Labels))
	for i, v := range pr.Labels {
		labels[i] = v.GetName()
	}

	newPRMessage, err := renderTemplate("newPR", event)
	if err != nil {
		p.API.LogWarn("Failed to render template", "error", err.Error())
		return
	}

	closedPRMessage, err := renderTemplate("closedPR", event)
	if err != nil {
		p.API.LogWarn("Failed to render template", "error", err.Error())
		return
	}

	post := &model.Post{
		UserId: p.BotUserID,
		Type:   "custom_git_pr",
	}

	for _, sub := range subs {
		if !sub.Pulls() && !sub.PullsMerged() {
			continue
		}

		if sub.PullsMerged() && action != actionClosed {
			continue
		}

		if p.excludeConfigOrgMember(event.GetSender(), sub) {
			continue
		}

		label := sub.Label()

		contained := false
		for _, v := range labels {
			if v == label {
				contained = true
			}
		}

		if !contained && label != "" {
			continue
		}

		if action == actionLabeled {
			if label != "" && label == eventLabel {
				pullRequestLabelledMessage, err := renderTemplate("pullRequestLabelled", event)
				if err != nil {
					p.API.LogWarn("Failed to render template", "error", err.Error())
					return
				}

				post.Message = pullRequestLabelledMessage
			} else {
				continue
			}
		}

		if action == actionOpened {
			post.Message = p.sanitizeDescription(newPRMessage)
		}

		if action == actionClosed {
			post.Message = closedPRMessage
		}

		post.ChannelId = sub.ChannelID
		if _, err := p.API.CreatePost(post); err != nil {
			p.API.LogWarn("Error webhook post", "post", post, "error", err.Error())
		}
	}
}
func (p *Plugin) sanitizeDescription(description string) string {
	var policy = bluemonday.StrictPolicy()
	policy.SkipElementsContent("details")
	return strings.TrimSpace(policy.Sanitize(description))
}
func (p *Plugin) handlePRDescriptionMentionNotification(event *github.PullRequestEvent) {
	action := event.GetAction()
	if action != actionOpened {
		return
	}

	body := event.GetPullRequest().GetBody()

	mentionedUsernames := parseGitHubUsernamesFromText(body)

	message, err := renderTemplate("pullRequestMentionNotification", event)
	if err != nil {
		p.API.LogWarn("Failed to render template", "error", err.Error())
		return
	}

	post := &model.Post{
		UserId:  p.BotUserID,
		Message: message,
		Type:    "custom_git_mention",
	}

	for _, username := range mentionedUsernames {
		// Don't notify user of their own comment
		if username == event.GetSender().GetLogin() {
			continue
		}

		// Notifications for pull request authors are handled separately
		if username == event.GetPullRequest().GetUser().GetLogin() {
			continue
		}

		userID := p.getGitHubToUserIDMapping(username)
		if userID == "" {
			continue
		}

		if event.GetRepo().GetPrivate() && !p.permissionToRepo(userID, event.GetRepo().GetFullName()) {
			continue
		}

		channel, err := p.API.GetDirectChannel(userID, p.BotUserID)
		if err != nil {
			continue
		}

		post.ChannelId = channel.Id

		if _, err = p.API.CreatePost(post); err != nil {
			p.API.LogWarn("Error webhook post", "post", post, "error", err.Error())
		}

		p.sendRefreshEvent(userID)
	}
}

func (p *Plugin) postIssueEvent(event *github.IssuesEvent) {
	repo := event.GetRepo()
	issue := event.GetIssue()
	action := event.GetAction()

	// This condition is made to check if the message doesn't get automatically labeled to prevent duplicated issue messages
	timeDiff := time.Until(issue.GetCreatedAt()) * -1
	if action == actionLabeled && timeDiff.Seconds() < 4.00 {
		return
	}

	subscribedChannels := p.GetSubscribedChannelsForRepository(repo)
	if len(subscribedChannels) == 0 {
		return
	}

	issueTemplate := ""
	switch action {
	case actionOpened:
		issueTemplate = "newIssue"

	case actionClosed:
		issueTemplate = "closedIssue"

	case actionReopened:
		issueTemplate = "reopenedIssue"

	case actionLabeled:
		issueTemplate = "issueLabelled"

	default:
		return
	}

	renderedMessage, err := renderTemplate(issueTemplate, event)
	if err != nil {
		p.API.LogWarn("Failed to render template", "error", err.Error())
		return
	}
	renderedMessage = p.sanitizeDescription(renderedMessage)

	post := &model.Post{
		UserId:  p.BotUserID,
		Type:    "custom_git_issue",
		Message: renderedMessage,
	}

	eventLabel := event.GetLabel().GetName()
	labels := make([]string, len(issue.Labels))
	for i, v := range issue.Labels {
		labels[i] = v.GetName()
	}

	for _, sub := range subscribedChannels {
		if !sub.Issues() && !sub.IssueCreations() {
			continue
		}

		if sub.IssueCreations() && action != actionOpened {
			continue
		}

		if p.excludeConfigOrgMember(event.GetSender(), sub) {
			continue
		}

		label := sub.Label()

		contained := false
		for _, v := range labels {
			if v == label {
				contained = true
			}
		}

		if !contained && label != "" {
			continue
		}

		if action == actionLabeled {
			if label == "" || label != eventLabel {
				continue
			}
		}

		post.ChannelId = sub.ChannelID
		if _, err := p.API.CreatePost(post); err != nil {
			p.API.LogWarn("Error webhook post", "post", post, "error", err.Error())
		}
	}
}

func (p *Plugin) postPushEvent(event *github.PushEvent) {
	repo := event.GetRepo()

	subs := p.GetSubscribedChannelsForRepository(ConvertPushEventRepositoryToRepository(repo))

	if len(subs) == 0 {
		return
	}

	commits := event.Commits
	if len(commits) == 0 {
		return
	}

	pushedCommitsMessage, err := renderTemplate("pushedCommits", event)
	if err != nil {
		p.API.LogWarn("Failed to render template", "error", err.Error())
		return
	}

	post := &model.Post{
		UserId:  p.BotUserID,
		Type:    "custom_git_push",
		Message: pushedCommitsMessage,
	}

	for _, sub := range subs {
		if !sub.Pushes() {
			continue
		}

		if p.excludeConfigOrgMember(event.GetSender(), sub) {
			continue
		}

		post.ChannelId = sub.ChannelID
		if _, err := p.API.CreatePost(post); err != nil {
			p.API.LogWarn("Error webhook post", "post", post, "error", err.Error())
		}
	}
}

func (p *Plugin) postCreateEvent(event *github.CreateEvent) {
	repo := event.GetRepo()

	subs := p.GetSubscribedChannelsForRepository(repo)
	if len(subs) == 0 {
		return
	}

	typ := event.GetRefType()
	if typ != "tag" && typ != "branch" {
		return
	}

	newCreateMessage, err := renderTemplate("newCreateMessage", event)
	if err != nil {
		p.API.LogWarn("Failed to render template", "error", err.Error())
		return
	}

	post := &model.Post{
		UserId:  p.BotUserID,
		Type:    "custom_git_create",
		Message: newCreateMessage,
	}

	for _, sub := range subs {
		if !sub.Creates() {
			continue
		}

		if p.excludeConfigOrgMember(event.GetSender(), sub) {
			continue
		}

		post.ChannelId = sub.ChannelID
		if _, err := p.API.CreatePost(post); err != nil {
			p.API.LogWarn("Error webhook post", "post", post, "error", err.Error())
		}
	}
}

func (p *Plugin) postDeleteEvent(event *github.DeleteEvent) {
	repo := event.GetRepo()

	subs := p.GetSubscribedChannelsForRepository(repo)

	if len(subs) == 0 {
		return
	}

	typ := event.GetRefType()

	if typ != "tag" && typ != "branch" {
		return
	}

	newDeleteMessage, err := renderTemplate("newDeleteMessage", event)
	if err != nil {
		p.API.LogWarn("Failed to render template", "error", err.Error())
		return
	}

	post := &model.Post{
		UserId:  p.BotUserID,
		Type:    "custom_git_delete",
		Message: newDeleteMessage,
	}

	for _, sub := range subs {
		if !sub.Deletes() {
			continue
		}

		if p.excludeConfigOrgMember(event.GetSender(), sub) {
			continue
		}

		post.ChannelId = sub.ChannelID
		if _, err := p.API.CreatePost(post); err != nil {
			p.API.LogWarn("Error webhook post", "post", post, "error", err.Error())
		}
	}
}

func (p *Plugin) postIssueCommentEvent(event *github.IssueCommentEvent) {
	repo := event.GetRepo()

	subs := p.GetSubscribedChannelsForRepository(repo)

	if len(subs) == 0 {
		return
	}

	if event.GetAction() != actionCreated {
		return
	}

	message, err := renderTemplate("issueComment", event)
	if err != nil {
		p.API.LogWarn("Failed to render template", "error", err.Error())
		return
	}

	post := &model.Post{
		UserId: p.BotUserID,
		Type:   "custom_git_comment",
	}

	labels := make([]string, len(event.GetIssue().Labels))
	for i, v := range event.GetIssue().Labels {
		labels[i] = v.GetName()
	}

	for _, sub := range subs {
		if !sub.IssueComments() {
			continue
		}

		if p.excludeConfigOrgMember(event.GetSender(), sub) {
			continue
		}

		label := sub.Label()

		contained := false
		for _, v := range labels {
			if v == label {
				contained = true
			}
		}

		if !contained && label != "" {
			continue
		}

		if event.GetAction() == actionCreated {
			post.Message = message
		}

		post.ChannelId = sub.ChannelID

		if _, err := p.API.CreatePost(post); err != nil {
			p.API.LogWarn("Error webhook post", "post", post, "error", err.Error())
		}
	}
}

func (p *Plugin) senderMutedByReceiver(userID string, sender string) bool {
	mutedUsernameBytes, _ := p.API.KVGet(userID + "-muted-users")
	mutedUsernames := string(mutedUsernameBytes)
	return strings.Contains(mutedUsernames, sender)
}

func (p *Plugin) postPullRequestReviewEvent(event *github.PullRequestReviewEvent) {
	repo := event.GetRepo()

	subs := p.GetSubscribedChannelsForRepository(repo)
	if len(subs) == 0 {
		return
	}

	action := event.GetAction()
	if action != actionSubmitted {
		return
	}

	switch event.GetReview().GetState() {
	case "APPROVED":
	case "COMMENTED":
	case "CHANGES_REQUESTED":
	default:
		p.API.LogDebug("Unhandled review state", "state", event.GetReview().GetState())
		return
	}

	newReviewMessage, err := renderTemplate("pullRequestReviewEvent", event)
	if err != nil {
		p.API.LogWarn("Failed to render template", "error", err.Error())
		return
	}

	post := &model.Post{
		UserId:  p.BotUserID,
		Type:    "custom_git_pull_review",
		Message: newReviewMessage,
	}

	labels := make([]string, len(event.GetPullRequest().Labels))
	for i, v := range event.GetPullRequest().Labels {
		labels[i] = v.GetName()
	}

	for _, sub := range subs {
		if !sub.PullReviews() {
			continue
		}

		if p.excludeConfigOrgMember(event.GetSender(), sub) {
			continue
		}

		label := sub.Label()

		contained := false
		for _, v := range labels {
			if v == label {
				contained = true
			}
		}

		if !contained && label != "" {
			continue
		}

		post.ChannelId = sub.ChannelID
		if _, err := p.API.CreatePost(post); err != nil {
			p.API.LogWarn("Error webhook post", "post", post, "error", err.Error())
		}
	}
}

func (p *Plugin) postPullRequestReviewCommentEvent(event *github.PullRequestReviewCommentEvent) {
	repo := event.GetRepo()

	subs := p.GetSubscribedChannelsForRepository(repo)
	if len(subs) == 0 {
		return
	}

	newReviewMessage, err := renderTemplate("newReviewComment", event)
	if err != nil {
		p.API.LogWarn("Failed to render template", "error", err.Error())
		return
	}

	post := &model.Post{
		UserId:  p.BotUserID,
		Type:    "custom_git_pull_review_comment",
		Message: newReviewMessage,
	}

	labels := make([]string, len(event.GetPullRequest().Labels))
	for i, v := range event.GetPullRequest().Labels {
		labels[i] = v.GetName()
	}

	for _, sub := range subs {
		if !sub.PullReviews() {
			continue
		}

		if p.excludeConfigOrgMember(event.GetSender(), sub) {
			continue
		}

		label := sub.Label()

		contained := false
		for _, v := range labels {
			if v == label {
				contained = true
			}
		}

		if !contained && label != "" {
			continue
		}

		post.ChannelId = sub.ChannelID
		if _, err := p.API.CreatePost(post); err != nil {
			p.API.LogWarn("Error webhook post", "post", post, "error", err.Error())
		}
	}
}

func (p *Plugin) handleCommentMentionNotification(event *github.IssueCommentEvent) {
	action := event.GetAction()
	if action == actionEdited || action == actionDeleted {
		return
	}

	body := event.GetComment().GetBody()

	// Try to parse out email footer junk
	if strings.Contains(body, "notifications@github.com") {
		body = strings.Split(body, "\n\nOn")[0]
	}

	mentionedUsernames := parseGitHubUsernamesFromText(body)

	message, err := renderTemplate("commentMentionNotification", event)
	if err != nil {
		p.API.LogWarn("Failed to render template", "error", err.Error())
		return
	}

	post := &model.Post{
		UserId:  p.BotUserID,
		Message: message,
		Type:    "custom_git_mention",
	}

	for _, username := range mentionedUsernames {
		// Don't notify user of their own comment
		if username == event.GetSender().GetLogin() {
			continue
		}

		// Notifications for issue authors are handled separately
		if username == event.GetIssue().GetUser().GetLogin() {
			continue
		}

		userID := p.getGitHubToUserIDMapping(username)
		if userID == "" {
			continue
		}

		if event.GetRepo().GetPrivate() && !p.permissionToRepo(userID, event.GetRepo().GetFullName()) {
			continue
		}

		channel, err := p.API.GetDirectChannel(userID, p.BotUserID)
		if err != nil {
			continue
		}

		post.ChannelId = channel.Id
		if _, err = p.API.CreatePost(post); err != nil {
			p.API.LogWarn("Error creating mention post", "error", err.Error())
		}

		p.sendRefreshEvent(userID)
	}
}

func (p *Plugin) handleCommentAuthorNotification(event *github.IssueCommentEvent) {
	author := event.GetIssue().GetUser().GetLogin()
	if author == event.GetSender().GetLogin() {
		return
	}

	action := event.GetAction()
	if action == actionEdited || action == actionDeleted {
		return
	}

	authorUserID := p.getGitHubToUserIDMapping(author)
	if authorUserID == "" {
		return
	}

	if event.GetRepo().GetPrivate() && !p.permissionToRepo(authorUserID, event.GetRepo().GetFullName()) {
		return
	}

	splitURL := strings.Split(event.GetIssue().GetHTMLURL(), "/")
	if len(splitURL) < 2 {
		return
	}

	var templateName string
	switch splitURL[len(splitURL)-2] {
	case "pull":
		templateName = "commentAuthorPullRequestNotification"
	case "issues":
		templateName = "commentAuthorIssueNotification"
	default:
		p.API.LogDebug("Unhandled issue type", "type", splitURL[len(splitURL)-2])
		return
	}

	if p.senderMutedByReceiver(authorUserID, event.GetSender().GetLogin()) {
		p.API.LogDebug("Commenter is muted, skipping notification")
		return
	}

	message, err := renderTemplate(templateName, event)
	if err != nil {
		p.API.LogWarn("Failed to render template", "error", err.Error())
		return
	}

	p.CreateBotDMPost(authorUserID, message, "custom_git_author")
	p.sendRefreshEvent(authorUserID)
}

func (p *Plugin) handleCommentAssigneeNotification(event *github.IssueCommentEvent) {
	author := event.GetIssue().GetUser().GetLogin()
	assignees := event.GetIssue().Assignees
	repoName := event.GetRepo().GetFullName()

	splitURL := strings.Split(event.GetIssue().GetHTMLURL(), "/")
	if len(splitURL) < 2 {
		return
	}
	var templateName string
	switch splitURL[len(splitURL)-2] {
	case "pull":
		templateName = "commentAssigneePullRequestNotification"
	case "issues":
		templateName = "commentAssigneeIssueNotification"
	default:
		p.API.LogWarn("Unhandled issue type", "type", splitURL[len(splitURL)-2])
		return
	}

	for _, assignee := range assignees {
		userID := p.getGitHubToUserIDMapping(assignee.GetLogin())
		if userID == "" {
			continue
		}

		if author == assignee.GetLogin() {
			continue
		}
		if event.Sender.GetLogin() == assignee.GetLogin() {
			continue
		}

		if !p.permissionToRepo(userID, repoName) {
			continue
		}

		assigneeID := p.getGitHubToUserIDMapping(assignee.GetLogin())
		if assigneeID == "" {
			continue
		}

		if p.senderMutedByReceiver(assigneeID, event.GetSender().GetLogin()) {
			p.API.LogDebug("Commenter is muted, skipping notification")
			continue
		}

		message, err := renderTemplate(templateName, event)
		if err != nil {
			p.API.LogWarn("Failed to render template", "error", err.Error())
			continue
		}
		p.CreateBotDMPost(assigneeID, message, "custom_git_assignee")
		p.sendRefreshEvent(assigneeID)
	}
}

func (p *Plugin) handlePullRequestNotification(event *github.PullRequestEvent) {
	author := event.GetPullRequest().GetUser().GetLogin()
	sender := event.GetSender().GetLogin()
	repoName := event.GetRepo().GetFullName()
	isPrivate := event.GetRepo().GetPrivate()

	requestedReviewer := ""
	requestedUserID := ""
	authorUserID := ""
	assigneeUserID := ""

	switch event.GetAction() {
	case "review_requested":
		requestedReviewer = event.GetRequestedReviewer().GetLogin()
		if requestedReviewer == sender {
			return
		}
		requestedUserID = p.getGitHubToUserIDMapping(requestedReviewer)
		if isPrivate && !p.permissionToRepo(requestedUserID, repoName) {
			requestedUserID = ""
		}
	case actionClosed:
		if author == sender {
			return
		}
		authorUserID = p.getGitHubToUserIDMapping(author)
		if isPrivate && !p.permissionToRepo(authorUserID, repoName) {
			authorUserID = ""
		}
	case actionReopened:
		if author == sender {
			return
		}
		authorUserID = p.getGitHubToUserIDMapping(author)
		if isPrivate && !p.permissionToRepo(authorUserID, repoName) {
			authorUserID = ""
		}
	case actionAssigned:
		assignee := event.GetPullRequest().GetAssignee().GetLogin()
		if assignee == sender {
			return
		}
		assigneeUserID = p.getGitHubToUserIDMapping(assignee)
		if isPrivate && !p.permissionToRepo(assigneeUserID, repoName) {
			assigneeUserID = ""
		}
	default:
		p.API.LogDebug("Unhandled event action", "action", event.GetAction())
		return
	}

	message, err := renderTemplate("pullRequestNotification", event)
	if err != nil {
		p.API.LogWarn("Failed to render template", "error", err.Error())
		return
	}

	if len(requestedUserID) > 0 {
		p.CreateBotDMPost(requestedUserID, message, "custom_git_review_request")
		p.sendRefreshEvent(requestedUserID)
	}

	p.postIssueNotification(message, authorUserID, assigneeUserID)
}

func (p *Plugin) handleIssueNotification(event *github.IssuesEvent) {
	author := event.GetIssue().GetUser().GetLogin()
	sender := event.GetSender().GetLogin()
	if author == sender {
		return
	}
	repoName := event.GetRepo().GetFullName()
	isPrivate := event.GetRepo().GetPrivate()

	message := ""
	authorUserID := ""
	assigneeUserID := ""

	switch event.GetAction() {
	case actionClosed:
		authorUserID = p.getGitHubToUserIDMapping(author)
		if isPrivate && !p.permissionToRepo(authorUserID, repoName) {
			authorUserID = ""
		}
	case actionReopened:
		authorUserID = p.getGitHubToUserIDMapping(author)
		if isPrivate && !p.permissionToRepo(authorUserID, repoName) {
			authorUserID = ""
		}
	case actionAssigned:
		assignee := event.GetAssignee().GetLogin()
		if assignee == sender {
			return
		}
		assigneeUserID = p.getGitHubToUserIDMapping(assignee)
		if isPrivate && !p.permissionToRepo(assigneeUserID, repoName) {
			assigneeUserID = ""
		}
	default:
		p.API.LogDebug("Unhandled event action", "action", event.GetAction())
		return
	}

	message, err := renderTemplate("issueNotification", event)
	if err != nil {
		p.API.LogWarn("Failed to render template", "error", err.Error())
		return
	}

	p.postIssueNotification(message, authorUserID, assigneeUserID)
}

func (p *Plugin) postIssueNotification(message, authorUserID, assigneeUserID string) {
	if len(authorUserID) > 0 {
		p.CreateBotDMPost(authorUserID, message, "custom_git_author")
		p.sendRefreshEvent(authorUserID)
	}

	if len(assigneeUserID) > 0 {
		p.CreateBotDMPost(assigneeUserID, message, "custom_git_assigned")
		p.sendRefreshEvent(assigneeUserID)
	}
}

func (p *Plugin) handlePullRequestReviewNotification(event *github.PullRequestReviewEvent) {
	author := event.GetPullRequest().GetUser().GetLogin()
	if author == event.GetSender().GetLogin() {
		return
	}

	if event.GetAction() != actionSubmitted {
		return
	}

	authorUserID := p.getGitHubToUserIDMapping(author)
	if authorUserID == "" {
		return
	}

	if event.GetRepo().GetPrivate() && !p.permissionToRepo(authorUserID, event.GetRepo().GetFullName()) {
		return
	}

	message, err := renderTemplate("pullRequestReviewNotification", event)
	if err != nil {
		p.API.LogWarn("Failed to render template", "error", err.Error())
		return
	}

	p.CreateBotDMPost(authorUserID, message, "custom_git_review")
	p.sendRefreshEvent(authorUserID)
}

func (p *Plugin) postStarEvent(event *github.StarEvent) {
	repo := event.GetRepo()

	subs := p.GetSubscribedChannelsForRepository(repo)

	if len(subs) == 0 {
		return
	}

	newStarMessage, err := renderTemplate("newRepoStar", event)
	if err != nil {
		p.API.LogWarn("Failed to render template", "error", err.Error())
		return
	}

	post := &model.Post{
		UserId:  p.BotUserID,
		Type:    "custom_git_star",
		Message: newStarMessage,
	}

	for _, sub := range subs {
		if !sub.Stars() {
			continue
		}

		if p.excludeConfigOrgMember(event.GetSender(), sub) {
			continue
		}

		post.ChannelId = sub.ChannelID
		if _, err := p.API.CreatePost(post); err != nil {
			p.API.LogWarn("Error webhook post", "post", post, "error", err.Error())
		}
	}
}
