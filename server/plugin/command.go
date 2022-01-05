package plugin

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"unicode"

	"github.com/google/go-github/v37/github"
	"github.com/mattermost/mattermost-plugin-api/experimental/command"
	"github.com/mattermost/mattermost-server/v6/model"
	"github.com/mattermost/mattermost-server/v6/plugin"
	"github.com/pkg/errors"
)

const (
	featureIssueCreation = "issue_creations"
	featureIssues        = "issues"
	featurePulls         = "pulls"
	featurePushes        = "pushes"
	featureCreates       = "creates"
	featureDeletes       = "deletes"
	featureIssueComments = "issue_comments"
	featurePullReviews   = "pull_reviews"
	featureStars         = "stars"
)

var validFeatures = map[string]bool{
	featureIssueCreation: true,
	featureIssues:        true,
	featurePulls:         true,
	featurePushes:        true,
	featureCreates:       true,
	featureDeletes:       true,
	featureIssueComments: true,
	featurePullReviews:   true,
	featureStars:         true,
}

const (
	subCommandList      = "list"
	subCommandView      = "view"
	subCommandAdd       = "add"
	subCommandDelete    = "delete"
	subCommandDeleteAll = "delete-all"
)

var webhookEvents = []string{"create", "delete", "issue_comment", "issues", "pull_request", "pull_request_review", "pull_request_review_comment", "push", "star"}

const (
	githubHookURL = "/settings/hooks/"
)

// validateFeatures returns false when 1 or more given features
// are invalid along with a list of the invalid features.
func validateFeatures(features []string) (bool, []string) {
	valid := true
	invalidFeatures := []string{}
	hasLabel := false
	for _, f := range features {
		if _, ok := validFeatures[f]; ok {
			continue
		}
		if strings.HasPrefix(f, "label") {
			hasLabel = true
			continue
		}
		invalidFeatures = append(invalidFeatures, f)
		valid = false
	}
	if valid && hasLabel {
		// must have "pulls" or "issues" in features when using a label
		for _, f := range features {
			if f == featurePulls || f == featureIssues {
				return valid, invalidFeatures
			}
		}
		valid = false
	}
	return valid, invalidFeatures
}

func (p *Plugin) getCommand(config *Configuration) (*model.Command, error) {
	iconData, err := command.GetIconData(p.API, "assets/icon-bg.svg")
	if err != nil {
		return nil, errors.Wrap(err, "failed to get icon data")
	}

	return &model.Command{
		Trigger:              "github",
		AutoComplete:         true,
		AutoCompleteDesc:     "Available commands: connect, disconnect, todo, me, settings, subscribe, unsubscribe, mute, help, issue , webhook",
		AutoCompleteHint:     "[command]",
		AutocompleteData:     getAutocompleteData(config),
		AutocompleteIconData: iconData,
	}, nil
}

func (p *Plugin) postCommandResponse(args *model.CommandArgs, text string) {
	post := &model.Post{
		UserId:    p.BotUserID,
		ChannelId: args.ChannelId,
		RootId:    args.RootId,
		Message:   text,
	}
	_ = p.API.SendEphemeralPost(args.UserId, post)
}

func (p *Plugin) getMutedUsernames(userInfo *GitHubUserInfo) []string {
	mutedUsernameBytes, err := p.API.KVGet(userInfo.UserID + "-muted-users")
	if err != nil {
		return nil
	}
	mutedUsernames := string(mutedUsernameBytes)
	var mutedUsers []string
	if len(mutedUsernames) == 0 {
		return mutedUsers
	}
	mutedUsers = strings.Split(mutedUsernames, ",")
	return mutedUsers
}

func (p *Plugin) handleMuteList(args *model.CommandArgs, userInfo *GitHubUserInfo) string {
	mutedUsernames := p.getMutedUsernames(userInfo)
	var mutedUsers string
	for _, user := range mutedUsernames {
		mutedUsers += fmt.Sprintf("- %v\n", user)
	}
	if len(mutedUsers) == 0 {
		return "You have no muted users"
	}
	return "Your muted users:\n" + mutedUsers
}

func contains(s []string, e string) (bool, int) {
	for index, a := range s {
		if a == e {
			return true, index
		}
	}
	return false, -1
}

func (p *Plugin) handleMuteAdd(args *model.CommandArgs, username string, userInfo *GitHubUserInfo) string {
	mutedUsernames := p.getMutedUsernames(userInfo)
	if userContains, _ := contains(mutedUsernames, username); userContains {
		return username + " is already muted"
	}

	if strings.Contains(username, ",") {
		return "Invalid username provided"
	}

	var mutedUsers string
	if len(mutedUsernames) > 0 {
		// , is a character not allowed in github usernames so we can split on them
		mutedUsers = strings.Join(mutedUsernames, ",") + "," + username
	} else {
		mutedUsers = username
	}
	if err := p.API.KVSet(userInfo.UserID+"-muted-users", []byte(mutedUsers)); err != nil {
		return "Error occurred saving list of muted users"
	}
	return fmt.Sprintf("`%v`", username) + " is now muted. You will no longer receive notifications for comments in your PRs and issues."
}

func (p *Plugin) handleUnmute(args *model.CommandArgs, username string, userInfo *GitHubUserInfo) string {
	mutedUsernames := p.getMutedUsernames(userInfo)
	userToMute := []string{username}
	newMutedList := arrayDifference(mutedUsernames, userToMute)
	if err := p.API.KVSet(userInfo.UserID+"-muted-users", []byte(strings.Join(newMutedList, ","))); err != nil {
		return "Error occurred unmuting users"
	}
	return fmt.Sprintf("`%v`", username) + " is no longer muted"
}

func (p *Plugin) handleUnmuteAll(args *model.CommandArgs, userInfo *GitHubUserInfo) string {
	if err := p.API.KVSet(userInfo.UserID+"-muted-users", []byte("")); err != nil {
		return "Error occurred unmuting users"
	}
	return "Unmuted all users"
}

func (p *Plugin) handleMuteCommand(_ *plugin.Context, args *model.CommandArgs, parameters []string, userInfo *GitHubUserInfo) string {
	if len(parameters) == 0 {
		return "Invalid mute command. Available commands are 'list', 'add' and 'delete'."
	}

	command := parameters[0]

	switch {
	case command == subCommandList:
		return p.handleMuteList(args, userInfo)
	case command == subCommandAdd:
		if len(parameters) != 2 {
			return "Invalid number of parameters supplied to " + command
		}
		return p.handleMuteAdd(args, parameters[1], userInfo)
	case command == subCommandDelete:
		if len(parameters) != 2 {
			return "Invalid number of parameters supplied to " + command
		}
		return p.handleUnmute(args, parameters[1], userInfo)
	case command == subCommandDeleteAll:
		return p.handleUnmuteAll(args, userInfo)
	default:
		return fmt.Sprintf("Unknown subcommand %v", command)
	}
}

// Returns the elements in a, that are not in b
func arrayDifference(a, b []string) []string {
	mb := make(map[string]struct{}, len(b))
	for _, x := range b {
		mb[x] = struct{}{}
	}
	var diff []string
	for _, x := range a {
		if _, found := mb[x]; !found {
			diff = append(diff, x)
		}
	}
	return diff
}

func (p *Plugin) handleSubscribe(c *plugin.Context, args *model.CommandArgs, parameters []string, userInfo *GitHubUserInfo) string {
	switch {
	case len(parameters) == 0:
		return "Please specify a repository or 'list' command."
	case len(parameters) == 1 && parameters[0] == subCommandList:
		return p.handleSubscriptionsList(c, args, parameters[1:], userInfo)
	default:
		return p.handleSubscribesAdd(c, args, parameters, userInfo)
	}
}

func (p *Plugin) handleSubscriptions(c *plugin.Context, args *model.CommandArgs, parameters []string, userInfo *GitHubUserInfo) string {
	if len(parameters) == 0 {
		return "Invalid subscribe command. Available commands are 'list', 'add' and 'delete'."
	}

	command := parameters[0]
	parameters = parameters[1:]

	switch {
	case command == subCommandList:
		return p.handleSubscriptionsList(c, args, parameters, userInfo)
	case command == subCommandAdd:
		return p.handleSubscribesAdd(c, args, parameters, userInfo)
	case command == subCommandDelete:
		return p.handleUnsubscribe(c, args, parameters, userInfo)
	default:
		return fmt.Sprintf("Unknown subcommand %v", command)
	}
}

func (p *Plugin) handleSubscriptionsList(_ *plugin.Context, args *model.CommandArgs, parameters []string, _ *GitHubUserInfo) string {
	txt := ""
	subs, err := p.GetSubscriptionsByChannel(args.ChannelId)
	if err != nil {
		return err.Error()
	}

	if len(subs) == 0 {
		txt = "Currently there are no subscriptions in this channel"
	} else {
		txt = "### Subscriptions in this channel\n"
	}
	for _, sub := range subs {
		subFlags := sub.Flags.String()
		txt += fmt.Sprintf("* `%s` - %s", strings.Trim(sub.Repository, "/"), sub.Features)
		if subFlags != "" {
			txt += fmt.Sprintf(" %s", subFlags)
		}
		txt += "\n"
	}

	excludeRepos, err := p.GetExcludedNotificationRepos()
	if err != nil {
		return err.Error()
	}
	for _, repo := range excludeRepos {
		txt += fmt.Sprintf("* `%s` - %s", strings.Trim(repo, "/"), "notification : disabled")
		txt += "\n"
	}
	return txt
}

func (p *Plugin) handleSubscribesAdd(_ *plugin.Context, args *model.CommandArgs, parameters []string, userInfo *GitHubUserInfo) string {
	features := "pulls,issues,creates,deletes"
	flags := SubscriptionFlags{}

	var excludeRepo string
	if len(parameters) > 1 {
		var optionList []string

		for _, element := range parameters[1:] {
			switch {
			case isFlag(element):
				flags.AddFlag(parseFlag(element))
			case flags.ExcludeOrgRepos && excludeRepo == "":
				excludeRepo = element
			default:
				optionList = append(optionList, element)
			}
		}
		if len(optionList) > 1 {
			return "Just one list of features is allowed"
		} else if len(optionList) == 1 {
			features = optionList[0]
			fs := strings.Split(features, ",")
			if SliceContainsString(fs, featureIssues) && SliceContainsString(fs, featureIssueCreation) {
				return "Feature list cannot contain both issue and issue_creations"
			}
			ok, ifs := validateFeatures(fs)
			if !ok {
				msg := fmt.Sprintf("Invalid feature(s) provided: %s", strings.Join(ifs, ","))
				if len(ifs) == 0 {
					msg = "Feature list must have \"pulls\" or \"issues\" when using a label."
				}
				return msg
			}
		}
	}

	ctx := context.Background()
	githubClient := p.githubConnectUser(ctx, userInfo)

	owner, repo := parseOwnerAndRepo(parameters[0], p.getBaseURL())
	if repo == "" {
		if err := p.SubscribeOrg(ctx, githubClient, args.UserId, owner, args.ChannelId, features, flags); err != nil {
			return err.Error()
		}
		orgLink := p.getBaseURL() + owner
		var subOrgMsg = fmt.Sprintf("Successfully subscribed to organization [%s](%s).", owner, orgLink)
		if flags.ExcludeOrgRepos {
			var excludeMsg string
			for _, value := range strings.Split(excludeRepo, ",") {
				val := strings.TrimSpace(value)
				notificationOffRepoOwner, NotificationOffRepo := parseOwnerAndRepo(val, p.getBaseURL())
				if notificationOffRepoOwner != owner {
					return fmt.Sprintf("--exclude repository  %s is not of subscribed organization .", NotificationOffRepo)
				}
				if err := p.StoreExcludedNotificationRepo(val); err != nil {
					return err.Error()
				}
				if excludeMsg != "" {
					excludeMsg += fmt.Sprintf(" and [%s](%s)", NotificationOffRepo, orgLink+"/"+NotificationOffRepo)
					continue
				}
				excludeMsg += fmt.Sprintf("[%s](%s)", NotificationOffRepo, orgLink+"/"+NotificationOffRepo)
			}
			subOrgMsg += "\n\n" + fmt.Sprintf("Notifications are disabled for %s", excludeMsg)
		}
		return subOrgMsg
	}
	if flags.ExcludeOrgRepos {
		return "--exclude feature currently support on organization level."
	}

	if err := p.Subscribe(ctx, githubClient, args.UserId, owner, repo, args.ChannelId, features, flags); err != nil {
		return err.Error()
	}

	msg := fmt.Sprintf("Successfully subscribed to %s.", repo)

	ghRepo, _, err := githubClient.Repositories.Get(ctx, owner, repo)
	if err != nil {
		p.API.LogWarn("Failed to fetch repository", "error", err.Error())
	} else if ghRepo != nil && ghRepo.GetPrivate() {
		msg += "\n\n**Warning:** You subscribed to a private repository. Anyone with access to this channel will be able to read the events getting posted here."
	}

	return msg
}

func (p *Plugin) handleUnsubscribe(_ *plugin.Context, args *model.CommandArgs, parameters []string, _ *GitHubUserInfo) string {
	if len(parameters) == 0 {
		return "Please specify a repository."
	}

	repo := parameters[0]

	if err := p.EnableNotificationTurnedOffRepo(repo); err != nil {
		p.API.LogWarn("Failed to unsubscribe while removing repo from disable notification list", "repo", repo, "error", err.Error())
		return "Encountered an error trying to remove from notify disabled list. Please try again."
	}
	if err := p.Unsubscribe(args.ChannelId, repo); err != nil {
		p.API.LogWarn("Failed to unsubscribe", "repo", repo, "error", err.Error())
		return "Encountered an error trying to unsubscribe. Please try again."
	}

	return fmt.Sprintf("Successfully unsubscribed from %s.", repo)
}

func (p *Plugin) handleDisconnect(_ *plugin.Context, args *model.CommandArgs, _ []string, _ *GitHubUserInfo) string {
	p.disconnectGitHubAccount(args.UserId)
	return "Disconnected your GitHub account."
}

func (p *Plugin) handleTodo(_ *plugin.Context, _ *model.CommandArgs, _ []string, userInfo *GitHubUserInfo) string {
	githubClient := p.githubConnectUser(context.Background(), userInfo)

	text, err := p.GetToDo(context.Background(), userInfo.GitHubUsername, githubClient)
	if err != nil {
		p.API.LogWarn("Failed get get Todos", "error", err.Error())
		return "Encountered an error getting your to do items."
	}

	return text
}

func (p *Plugin) handleMe(_ *plugin.Context, _ *model.CommandArgs, _ []string, userInfo *GitHubUserInfo) string {
	githubClient := p.githubConnectUser(context.Background(), userInfo)
	gitUser, _, err := githubClient.Users.Get(context.Background(), "")
	if err != nil {
		return "Encountered an error getting your GitHub profile."
	}

	text := fmt.Sprintf("You are connected to GitHub as:\n# [![image](%s =40x40)](%s) [%s](%s)", gitUser.GetAvatarURL(), gitUser.GetHTMLURL(), gitUser.GetLogin(), gitUser.GetHTMLURL())
	return text
}

func (p *Plugin) handleHelp(_ *plugin.Context, _ *model.CommandArgs, _ []string, _ *GitHubUserInfo) string {
	message, err := renderTemplate("helpText", p.getConfiguration())
	if err != nil {
		p.API.LogWarn("Failed to render help template", "error", err.Error())
		return "Encountered an error posting help text."
	}

	return "###### Mattermost GitHub Plugin - Slash Command Help\n" + message
}

func (p *Plugin) handleSettings(_ *plugin.Context, _ *model.CommandArgs, parameters []string, userInfo *GitHubUserInfo) string {
	if len(parameters) < 2 {
		return "Please specify both a setting and value. Use `/github help` for more usage information."
	}

	setting := parameters[0]
	settingValue := parameters[1]

	switch setting {
	case settingNotifications:
		switch settingValue {
		case settingOn:
			userInfo.Settings.Notifications = true
		case settingOff:
			userInfo.Settings.Notifications = false
		default:
			return "Invalid value. Accepted values are: \"on\" or \"off\"."
		}
	case settingReminders:
		switch settingValue {
		case settingOn:
			userInfo.Settings.DailyReminder = true
			userInfo.Settings.DailyReminderOnChange = false
		case settingOff:
			userInfo.Settings.DailyReminder = false
			userInfo.Settings.DailyReminderOnChange = false
		case settingOnChange:
			userInfo.Settings.DailyReminder = true
			userInfo.Settings.DailyReminderOnChange = true
		default:
			return "Invalid value. Accepted values are: \"on\" or \"off\" or \"on-change\" ."
		}
	default:
		return "Unknown setting " + setting
	}

	if setting == settingNotifications {
		if userInfo.Settings.Notifications {
			err := p.storeGitHubToUserIDMapping(userInfo.GitHubUsername, userInfo.UserID)
			if err != nil {
				p.API.LogWarn("Failed to store GitHub to userID mapping",
					"userID", userInfo.UserID,
					"GitHub username", userInfo.GitHubUsername,
					"error", err.Error())
			}
		} else {
			err := p.API.KVDelete(userInfo.GitHubUsername + githubUsernameKey)
			if err != nil {
				p.API.LogWarn("Failed to delete GitHub to userID mapping",
					"userID", userInfo.UserID,
					"GitHub username", userInfo.GitHubUsername,
					"error", err.Error())
			}
		}
	}

	err := p.storeGitHubUserInfo(userInfo)
	if err != nil {
		p.API.LogWarn("Failed to store github user info", "error", err.Error())
		return "Failed to store settings"
	}

	return "Settings updated."
}

func (p *Plugin) handleIssue(_ *plugin.Context, args *model.CommandArgs, parameters []string, userInfo *GitHubUserInfo) string {
	if len(parameters) == 0 {
		return "Invalid issue command. Available command is 'create'."
	}

	command := parameters[0]
	parameters = parameters[1:]

	switch {
	case command == "create":
		p.openIssueCreateModal(args.UserId, args.ChannelId, strings.Join(parameters, " "))
		return ""
	default:
		return fmt.Sprintf("Unknown subcommand %v", command)
	}
}

func (p *Plugin) handleWebhookAdd(_ *plugin.Context, parameters []string, args *model.CommandArgs, githubClient *github.Client, userInfo *GitHubUserInfo) string {
	if len(parameters) < 1 {
		return "Invalid parameter for add command, provide repo details in `owner[/repo]` format."
	}

	siteURL := p.getSiteURL()
	if strings.Contains(siteURL, "localhost") {
		return fmt.Sprintf("Your Site URL should be a public URL. You can configure the Site URL [here](%s/admin_console/environment/web_server)", *p.API.GetConfig().ServiceSettings.SiteURL)
	}
	baseURL := p.getBaseURL()
	owner, repo := parseOwnerAndRepo(parameters[0], baseURL)
	webhookExist, hookID, err := p.CheckWebhookExist(owner, repo, args.ChannelId)
	if err != nil {
		return err.Error()
	}

	if webhookExist {
		hookURL := baseURL
		label := owner
		if repo != "" {
			hookURL += owner + "/" + repo
			label += "/" + repo
		} else {
			hookURL += "organizations/" + owner
		}
		hookURL += githubHookURL
		return fmt.Sprintf("There is already a webhook for this repository that is pointing to this Mattermost server. Please delete the webhook from [%s](%s/%s) before running this command again.", label, hookURL, hookID)
	}
	var hook github.Hook
	var config = make(map[string]interface{})
	config["secret"] = p.getConfiguration().WebhookSecret
	config["url"] = siteURL + "/plugins/" + Manifest.Id + "/webhook"
	config["insecure_ssl"] = false
	config["content_type"] = "application/json"
	hook.Events = webhookEvents
	hook.Config = config
	ctx := context.Background()
	githubHook, _, err := p.CreateHook(ctx, githubClient, owner, repo, hook)
	if err != nil {
		if repo == "" {
			var scopes []string
			scopes, err = p.getOauthTokenScopes(userInfo.Token.AccessToken)
			if err != nil {
				return err.Error()
			}

			if exist, _ := findInSlice(scopes, string(github.ScopeAdminOrgHook)); !exist {
				return "insufficient OAuth token scope.\nPlease use the command `/github connect` to get the new scope."
			}
		}
		return err.Error()
	}

	hookURL := baseURL
	label := owner
	if repo != "" {
		hookURL += owner + "/" + repo
		label = owner + "/" + repo
	} else {
		hookURL += "organizations/" + owner
	}

	hookURL += githubHookURL
	txt := "Webhook Created Successfully \n"
	txt += fmt.Sprintf(" * [%s](%s%d)\n", label, hookURL, *githubHook.ID)
	return txt
}
func (p *Plugin) getOauthTokenScopes(token string) ([]string, error) {
	var scopes []string
	req, err := http.NewRequest("HEAD", "https://api.github.com/users/codertocat", nil)
	if err != nil {
		return scopes, err
	}
	req.Header.Set("Authorization", fmt.Sprintf("token %s", token))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return scopes, err
	}
	defer resp.Body.Close()
	for key, val := range resp.Header {
		if key == "X-Oauth-Scopes" {
			scopes = strings.Split(val[0], ",")
		}
	}
	return scopes, nil
}

func (p *Plugin) handleWebhookList(_ *plugin.Context, parameters []string, args *model.CommandArgs, githubClient *github.Client, userInfo *GitHubUserInfo) string {
	if len(parameters) == 0 {
		return "Invalid parameter for list command, provide repo details in `owner[/repo]` format."
	}
	if len(parameters) != 1 {
		return "Invalid parameter for list command, only accept one argument."
	}

	baseURL := p.getBaseURL()
	owner, repo := parseOwnerAndRepo(parameters[0], baseURL)

	var err error
	var txt = "### Webhooks in this Repository\n"
	ctx := context.Background()

	if repo == "" {
		txt = "### Webhooks in this Organization\n"
	}

	var githubHookList []*github.Hook
	opt := &github.ListOptions{
		PerPage: 50,
	}

	for {
		var githubHooks []*github.Hook
		var githubResponse *github.Response
		if repo == "" {
			githubHooks, githubResponse, err = githubClient.Organizations.ListHooks(ctx, owner, opt)
		} else {
			githubHooks, githubResponse, err = githubClient.Repositories.ListHooks(ctx, owner, repo, opt)
		}
		if err != nil {
			if repo == "" {
				var scopes []string
				var scopeError error
				scopes, scopeError = p.getOauthTokenScopes(userInfo.Token.AccessToken)
				if scopeError != nil {
					return scopeError.Error()
				}

				if exist, _ := findInSlice(scopes, string(github.ScopeAdminOrgHook)); !exist {
					return "insufficient OAuth token scope.\nPlease use the command `/github connect` to get the new scope."
				}
			}
			return err.Error()
		}
		githubHookList = append(githubHookList, githubHooks...)
		if githubResponse.NextPage == 0 {
			break
		}
		opt.Page = githubResponse.NextPage
	}

	for _, hook := range githubHookList {
		hookURL := baseURL
		label := owner
		if repo != "" {
			hookURL += owner + "/" + repo
			label += "/" + repo
		} else {
			hookURL += "organizations/" + owner
		}
		hookURL += githubHookURL

		if strings.Contains(hook.Config["url"].(string), p.getSiteURL()) {
			txt += fmt.Sprintf(" * [%s](%s%d) - points to Mattermost Server\n", label, hookURL, *hook.ID)
		} else {
			txt += fmt.Sprintf(" * [%s](%s%d)\n", label, hookURL, *hook.ID)
		}
	}

	if len(githubHookList) == 0 {
		if repo == "" {
			txt = fmt.Sprintf("There are currently no GitHub webhooks created for the [%s](https://github.com/%s) org.", owner, owner)
		} else {
			txt = fmt.Sprintf("There are currently no GitHub webhooks created for the [%s](https://github.com/%s) org or the [%s/%s](https://github.com/%s/%s) repository.", owner, owner, owner, repo, owner, repo)
		}
	}

	return txt
}

func findInSlice(slice []string, item string) (bool, int) {
	for index, value := range slice {
		if item == value {
			return true, index
		}
	}
	return false, -1
}

func (p *Plugin) handleWebhooks(c *plugin.Context, args *model.CommandArgs, parameters []string, userInfo *GitHubUserInfo) string {
	if len(parameters) == 0 {
		return "Please provide a subcommand `add` or `view`."
	}
	command := parameters[0]
	parameters = parameters[1:]
	ctx := context.Background()
	githubClient := p.githubConnectUser(ctx, userInfo)
	switch command {
	case subCommandAdd:
		return p.handleWebhookAdd(c, parameters, args, githubClient, userInfo)
	case subCommandView:
		return p.handleWebhookList(c, parameters, args, githubClient, userInfo)
	default:
		return fmt.Sprintf("Invalid subcommand `%s`.", command)
	}
}

func (p *Plugin) GetHook(ctx context.Context, githubClient *github.Client, owner, repo string, hookID int64) (*github.Hook, *github.Response, error) {
	if repo != "" {
		return githubClient.Repositories.GetHook(ctx, owner, repo, hookID)
	}
	return githubClient.Organizations.GetHook(ctx, owner, hookID)
}

func (p *Plugin) CreateHook(ctx context.Context, githubClient *github.Client, owner, repo string, hook github.Hook) (*github.Hook, *github.Response, error) {
	if repo != "" {
		return githubClient.Repositories.CreateHook(ctx, owner, repo, &hook)
	}

	return githubClient.Organizations.CreateHook(ctx, owner, &hook)
}

type CommandHandleFunc func(c *plugin.Context, args *model.CommandArgs, parameters []string, userInfo *GitHubUserInfo) string

func (p *Plugin) isAuthorizedSysAdmin(userID string) (bool, error) {
	user, appErr := p.API.GetUser(userID)
	if appErr != nil {
		return false, appErr
	}
	if !strings.Contains(user.Roles, "system_admin") {
		return false, nil
	}
	return true, nil
}

func (p *Plugin) ExecuteCommand(c *plugin.Context, args *model.CommandArgs) (*model.CommandResponse, *model.AppError) {
	config := p.getConfiguration()

	if err := config.IsValid(); err != nil {
		isSysAdmin, err := p.isAuthorizedSysAdmin(args.UserId)
		var text string
		switch {
		case err != nil:
			text = "Error checking user's permissions"
			p.API.LogWarn(text, "err", err.Error())
		case isSysAdmin:
			githubPluginURL := *p.API.GetConfig().ServiceSettings.SiteURL + "/admin_console/plugins/plugin_github"
			text = fmt.Sprintf("Before using this plugin, you will need to configure it by filling out the settings in the system console [here](%s). You can learn more about the setup process [here](%s).", githubPluginURL, "https://github.com/mattermost/mattermost-plugin-github#step-3-configure-the-plugin-in-mattermost")
		default:
			text = "Please contact your system administrator to configure the GitHub plugin."
		}

		p.postCommandResponse(args, text)
		return &model.CommandResponse{}, nil
	}

	command, action, parameters := parseCommand(args.Command)

	if command != "/github" {
		return &model.CommandResponse{}, nil
	}

	if action == "connect" {
		siteURL := p.API.GetConfig().ServiceSettings.SiteURL
		if siteURL == nil {
			p.postCommandResponse(args, "Encountered an error connecting to GitHub.")
			return &model.CommandResponse{}, nil
		}

		privateAllowed := p.getConfiguration().ConnectToPrivateByDefault
		if len(parameters) > 0 {
			if privateAllowed {
				p.postCommandResponse(args, fmt.Sprintf("Unknown command `%v`. Do you meant `/github connect`?", args.Command))
				return &model.CommandResponse{}, nil
			}

			if len(parameters) != 1 || parameters[0] != "private" {
				p.postCommandResponse(args, fmt.Sprintf("Unknown command `%v`. Do you meant `/github connect private`?", args.Command))
				return &model.CommandResponse{}, nil
			}

			privateAllowed = true
		}

		qparams := ""
		if privateAllowed {
			if !p.getConfiguration().EnablePrivateRepo {
				p.postCommandResponse(args, "Private repositories are disabled. Please ask a System Admin to enabled them.")
				return &model.CommandResponse{}, nil
			}
			qparams = "?private=true"
		}

		msg := fmt.Sprintf("[Click here to link your GitHub account.](%s/plugins/%s/oauth/connect%s)", *siteURL, Manifest.Id, qparams)
		p.postCommandResponse(args, msg)
		return &model.CommandResponse{}, nil
	}

	info, apiErr := p.getGitHubUserInfo(args.UserId)
	if apiErr != nil {
		text := "Unknown error."
		if apiErr.ID == apiErrorIDNotConnected {
			text = "You must connect your account to GitHub first. Either click on the GitHub logo in the bottom left of the screen or enter `/github connect`."
		}
		p.postCommandResponse(args, text)
		return &model.CommandResponse{}, nil
	}

	if f, ok := p.CommandHandlers[action]; ok {
		message := f(c, args, parameters, info)
		if message != "" {
			p.postCommandResponse(args, message)
		}
		return &model.CommandResponse{}, nil
	}

	p.postCommandResponse(args, fmt.Sprintf("Unknown action %v", action))
	return &model.CommandResponse{}, nil
}

func getAutocompleteData(config *Configuration) *model.AutocompleteData {
	github := model.NewAutocompleteData("github", "[command]", "Available commands: connect, disconnect, todo, subscribe, unsubscribe, me, settings")

	connect := model.NewAutocompleteData("connect", "", "Connect your Mattermost account to your GitHub account")
	if config.EnablePrivateRepo {
		if config.ConnectToPrivateByDefault {
			connect = model.NewAutocompleteData("connect", "", "Connect your Mattermost account to your GitHub account. Read access to your private repositories will be requested")
		} else {
			private := model.NewAutocompleteData("private", "(optional)", "If used, read access to your private repositories will be requested")
			connect.AddCommand(private)
		}
	}
	github.AddCommand(connect)

	disconnect := model.NewAutocompleteData("disconnect", "", "Disconnect your Mattermost account from your GitHub account")
	github.AddCommand(disconnect)

	help := model.NewAutocompleteData("help", "", "Display Slash Command help text")
	github.AddCommand(help)

	todo := model.NewAutocompleteData("todo", "", "Get a list of unread messages and pull requests awaiting your review")
	github.AddCommand(todo)

	subscriptions := model.NewAutocompleteData("subscriptions", "[command]", "Available commands: list, add, delete")

	subscribeList := model.NewAutocompleteData(subCommandList, "", "List the current channel subscriptions")
	subscriptions.AddCommand(subscribeList)

	subscriptionsAdd := model.NewAutocompleteData(subCommandAdd, "[owner/repo] [features] [flags]", "Subscribe the current channel to receive notifications about opened pull requests and issues for an organization or repository. [features] and [flags] are optional arguments")
	subscriptionsAdd.AddTextArgument("Owner/repo to subscribe to", "[owner/repo]", "")
	subscriptionsAdd.AddTextArgument("Comma-delimited list of one or more of: issues, pulls, pushes, creates, deletes, issue_creations, issue_comments, pull_reviews, label:\"<labelname>\". Defaults to pulls,issues,creates,deletes", "[features] (optional)", `/[^,-\s]+(,[^,-\s]+)*/`)
	if config.GitHubOrg != "" {
		exclude := []model.AutocompleteListItem{
			{
				HelpText: "notifications for these repos will be turned off",
				Hint:     "(optional)",
				Item:     "--exclude",
			},
		}
		subscriptionsAdd.AddStaticListArgument("Currently supports --exclude", true, exclude)
		subscriptionsAdd.AddTextArgument("Owner/repo to subscribe to", "[owner/repo]", "")
		flags := []model.AutocompleteListItem{
			{
				HelpText: "Events triggered by organization members will not be delivered (the organization config should be set, otherwise this flag has no effect)",
				Hint:     "(optional)",
				Item:     "--exclude-org-member",
			},
		}
		subscriptionsAdd.AddStaticListArgument("Currently supports --exclude-org-member ", false, flags)
	}
	subscriptions.AddCommand(subscriptionsAdd)

	subscriptionsDelete := model.NewAutocompleteData("delete", "[owner/repo]", "Unsubscribe the current channel from an organization or repository")
	subscriptionsDelete.AddTextArgument("Owner/repo to unsubscribe from", "[owner/repo]", "")
	subscriptions.AddCommand(subscriptionsDelete)

	github.AddCommand(subscriptions)

	me := model.NewAutocompleteData("me", "", "Display the connected GitHub account")
	github.AddCommand(me)

	mute := model.NewAutocompleteData("mute", "[command]", "Available commands: list, add, delete, delete-all")

	muteAdd := model.NewAutocompleteData(subCommandAdd, "[github username]", "Mute notifications from the provided GitHub user")
	muteAdd.AddTextArgument("GitHub user to mute", "[username]", "")
	mute.AddCommand(muteAdd)

	muteDelete := model.NewAutocompleteData("delete", "[github username]", "Unmute notifications from the provided GitHub user")
	muteDelete.AddTextArgument("GitHub user to unmute", "[username]", "")
	mute.AddCommand(muteDelete)

	github.AddCommand(mute)

	muteDeleteAll := model.NewAutocompleteData("delete-all", "", "Unmute all muted GitHub users")
	mute.AddCommand(muteDeleteAll)

	muteList := model.NewAutocompleteData(subCommandList, "", "List muted GitHub users")
	mute.AddCommand(muteList)

	settings := model.NewAutocompleteData("settings", "[setting] [value]", "Update your user settings")

	settingNotifications := model.NewAutocompleteData("notifications", "", "Turn notifications on/off")
	settingValue := []model.AutocompleteListItem{{
		HelpText: "Turn notifications on",
		Item:     "on",
	}, {
		HelpText: "Turn notifications off",
		Item:     "off",
	}}
	settingNotifications.AddStaticListArgument("", true, settingValue)
	settings.AddCommand(settingNotifications)

	remainderNotifications := model.NewAutocompleteData("reminders", "", "Turn notifications on/off")
	settingValue = []model.AutocompleteListItem{{
		HelpText: "Turn reminders on",
		Item:     "on",
	}, {
		HelpText: "Turn reminders off",
		Item:     "off",
	}, {
		HelpText: "Turn reminders on, but only get reminders if any changes have occurred since the previous day's reminder",
		Item:     settingOnChange,
	}}
	remainderNotifications.AddStaticListArgument("", true, settingValue)
	settings.AddCommand(remainderNotifications)

	github.AddCommand(settings)

	issue := model.NewAutocompleteData("issue", "[command]", "Available commands: create")

	issueCreate := model.NewAutocompleteData("create", "[title]", "Open a dialog to create a new issue in Github, using the title if provided")
	issueCreate.AddTextArgument("Title for the Github issue", "[title]", "")
	issue.AddCommand(issueCreate)

	github.AddCommand(issue)

	webhook := model.NewAutocompleteData("webhook", "[command]", "Available commands: add, view")

	webhookList := model.NewAutocompleteData(subCommandView, "owner[/repo]", "View webhooks or an organization or repository.")
	webhookList.AddTextArgument("Owner/repo to view webhooks from", "[owner/repo]", "")
	webhook.AddCommand(webhookList)
	webhookAdd := model.NewAutocompleteData(subCommandAdd, "owner[/repo]", "Add a webhook to desired owner[/repo]")
	webhookAdd.AddTextArgument("Organization or repository to list webhooks from", "owner[/repo]", "")
	webhook.AddCommand(webhookAdd)
	github.AddCommand(webhook)

	return github
}

// parseCommand parses the entire command input string and retrieves the command, action and parameters
func parseCommand(input string) (command, action string, parameters []string) {
	split := make([]string, 0)
	current := ""
	inQuotes := false

	for _, char := range input {
		if unicode.IsSpace(char) {
			// keep whitespaces that are inside double qoutes
			if inQuotes {
				current += " "
				continue
			}

			// ignore successive whitespaces that are outside of double quotes
			if len(current) == 0 && !inQuotes {
				continue
			}

			// append the current word to the list & move on to the next word/expression
			split = append(split, current)
			current = ""
			continue
		}

		// append the current character to the current word
		current += string(char)

		if char == '"' {
			inQuotes = !inQuotes
		}
	}

	// append the last word/expression to the list
	if len(current) > 0 {
		split = append(split, current)
	}

	command = split[0]

	if len(split) > 1 {
		action = split[1]
	}

	if len(split) > 2 {
		parameters = split[2:]
	}

	return command, action, parameters
}

func SliceContainsString(a []string, x string) bool {
	for _, n := range a {
		if x == n {
			return true
		}
	}
	return false
}
