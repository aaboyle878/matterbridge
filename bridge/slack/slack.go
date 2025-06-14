package bslack

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/42wim/matterbridge/bridge"
	"github.com/42wim/matterbridge/bridge/config"
	"github.com/42wim/matterbridge/bridge/helper"
	"github.com/42wim/matterbridge/matterhook"
	lru "github.com/hashicorp/golang-lru"
	"github.com/rs/xid"
	"github.com/slack-go/slack"
)

type Bslack struct {
	sync.RWMutex
	*bridge.Config

	mh *matterhook.Client
	sc *slack.Client

	cache        *lru.Cache
	uuid         string
	useChannelID bool
	eventServer  *http.Server

	channels *channels
	users    *users
	legacy   bool
}

const (
	sHello               = "hello"
	sChannelJoin         = "channel_join"
	sChannelLeave        = "channel_leave"
	sChannelJoined       = "channel_joined"
	sMemberJoined        = "member_joined_channel"
	sMessageChanged      = "message_changed"
	sMessageDeleted      = "message_deleted"
	sSlackAttachment     = "slack_attachment"
	sPinnedItem          = "pinned_item"
	sUnpinnedItem        = "unpinned_item"
	sChannelTopic        = "channel_topic"
	sChannelPurpose      = "channel_purpose"
	sFileComment         = "file_comment"
	sMeMessage           = "me_message"
	sUserTyping          = "user_typing"
	sLatencyReport       = "latency_report"
	sSystemUser          = "system"
	sSlackBotUser        = "slackbot"
	cfileDownloadChannel = "file_download_channel"

	tokenConfig           = "TokenBot"
	incomingWebhookConfig = "WebhookBindAddress"
	outgoingWebhookConfig = "WebhookURL"
	skipTLSConfig         = "SkipTLSVerify"
	useNickPrefixConfig   = "PrefixMessagesWithNick"
	editDisableConfig     = "EditDisable"
	editSuffixConfig      = "EditSuffix"
	iconURLConfig         = "iconurl"
	noSendJoinConfig      = "nosendjoinpart"
	messageLength         = 3000
)

func New(cfg *bridge.Config) bridge.Bridger {
	// Print a deprecation warning for legacy non-bot tokens (#527).
	token := cfg.GetString(tokenConfig)
	if token != "" && !strings.HasPrefix(token, "xoxb") {
		cfg.Log.Fatalf("Legacy user tokens are no longer supported. Please use a bot token (xoxb-...).")
	}
	return newBridge(cfg)
}

func newBridge(cfg *bridge.Config) *Bslack {
	newCache, err := lru.New(5000)
	if err != nil {
		cfg.Log.Fatalf("Could not create LRU cache for Slack bridge: %v", err)
	}
	b := &Bslack{
		Config: cfg,
		uuid:   xid.New().String(),
		cache:  newCache,
	}
	return b
}

func (b *Bslack) Command(cmd string) string {
	return ""
}

func (b *Bslack) Connect() error {
	b.RLock()
	defer b.RUnlock()

	if b.GetString(incomingWebhookConfig) == "" && b.GetString(outgoingWebhookConfig) == "" && b.GetString(tokenConfig) == "" {
		return errors.New("no connection method found: WebhookBindAddress, WebhookURL or Token need to be configured")
	}

	// If we have a token we use the Slack websocket-based RTM for both sending and receiving.
	token := b.GetString("TokenBot")
	if token == "" {
		token = b.GetString(tokenConfig)
	}
	if token != "" {
		b.Log.Info("Connecting using token")

		b.sc = slack.New(token, slack.OptionDebug(b.GetBool("Debug")))

		b.channels = newChannelManager(b.Log, b.sc)
		b.users = newUserManager(b.Log, b.sc)

		mux := http.NewServeMux()
		mux.HandleFunc("/slack/events", b.handleSlackEvents) // we'll define this function next

		b.eventServer = &http.Server{
			Addr:    ":3000", // you can make this configurable
			Handler: mux,
		}

		go func() {
			if err := b.eventServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				b.Log.Errorf("Slack event server error: %v", err)
			}
		}()

		/*
			b.rtm = b.sc.NewRTM()
			go b.rtm.ManageConnection()
			go b.handleSlack()
			return nil */
	}

	// In absence of a token we fall back to incoming and outgoing Webhooks.
	b.mh = matterhook.New(
		"",
		matterhook.Config{
			InsecureSkipVerify: b.GetBool("SkipTLSVerify"),
			DisableServer:      true,
		},
	)
	if b.GetString(outgoingWebhookConfig) != "" {
		b.Log.Info("Using specified webhook for outgoing messages.")
		b.mh.Url = b.GetString(outgoingWebhookConfig)
	}
	if b.GetString(incomingWebhookConfig) != "" {
		b.Log.Info("Setting up local webhook for incoming messages.")
		b.mh.BindAddress = b.GetString(incomingWebhookConfig)
		b.mh.DisableServer = false
		go b.handleSlack()
	}
	return nil
}

func (b *Bslack) Disconnect() error {
	if b.eventServer != nil {
		return b.eventServer.Close()
	}
	return nil
}

// JoinChannel only acts as a verification method that checks whether Matterbridge's
// Slack integration is already member of the channel. This is because Slack does not
// allow apps or bots to join channels themselves and they need to be invited
// manually by a user.
func (b *Bslack) JoinChannel(channel config.ChannelInfo) error {
	// We can only join a channel through the Slack API.
	if b.sc == nil {
		return nil
	}

	// try to join a channel when in legacy
	if b.legacy {
		_, _, _, err := b.sc.JoinConversation(channel.Name)
		if err != nil {
			switch err.Error() {
			case "name_taken", "restricted_action":
			case "default":
				return err
			}
		}
	}

	b.channels.populateChannels(false)

	channelInfo, err := b.channels.getChannel(channel.Name)
	if err != nil {
		return fmt.Errorf("could not join channel: %#v", err)
	}

	if strings.HasPrefix(channel.Name, "ID:") {
		b.useChannelID = true
		channel.Name = channelInfo.Name
	}

	// we can't join a channel unless we are using legacy tokens #651
	if !channelInfo.IsMember && !b.legacy {
		return fmt.Errorf("slack integration that matterbridge is using is not member of channel '%s', please add it manually", channelInfo.Name)
	}
	return nil
}

func (b *Bslack) Reload(cfg *bridge.Config) (string, error) {
	return "", nil
}

func (b *Bslack) Send(msg config.Message) (string, error) {
	// Too noisy to log like other events
	if msg.Event != config.EventUserTyping {
		b.Log.Debugf("=> Receiving %#v", msg)
	}

	msg.Text = helper.ClipMessage(msg.Text, messageLength, b.GetString("MessageClipped"))
	msg.Text = b.replaceCodeFence(msg.Text)

	// Make a action /me of the message
	if msg.Event == config.EventUserAction {
		msg.Text = "_" + msg.Text + "_"
	}

	// Use webhook to send the message
	if b.GetString(outgoingWebhookConfig) != "" && b.GetString(tokenConfig) == "" {
		return "", b.sendWebhook(msg)
	}
	return b.sendAPI(msg)
}

// sendWebhook uses the configured WebhookURL to send the message
func (b *Bslack) sendWebhook(msg config.Message) error {
	// Skip events.
	if msg.Event != "" {
		return nil
	}

	if b.GetBool(useNickPrefixConfig) {
		msg.Text = msg.Username + msg.Text
	}

	if msg.Extra != nil {
		// This sends a message only if we received a config.EVENT_FILE_FAILURE_SIZE.
		for _, rmsg := range helper.HandleExtra(&msg, b.General) {
			rmsg := rmsg // scopelint
			iconURL := config.GetIconURL(&rmsg, b.GetString(iconURLConfig))
			matterMessage := matterhook.OMessage{
				IconURL:  iconURL,
				Channel:  msg.Channel,
				UserName: rmsg.Username,
				Text:     rmsg.Text,
			}
			if err := b.mh.Send(matterMessage); err != nil {
				b.Log.Errorf("Failed to send message: %v", err)
			}
		}

		// Webhook doesn't support file uploads, so we add the URL manually.
		for _, f := range msg.Extra["file"] {
			fi, ok := f.(config.FileInfo)
			if !ok {
				b.Log.Errorf("Received a file with unexpected content: %#v", f)
				continue
			}
			if fi.URL != "" {
				msg.Text += " " + fi.URL
			}
		}
	}

	// If we have native slack_attachments add them.
	var attachs []slack.Attachment
	for _, attach := range msg.Extra[sSlackAttachment] {
		attachs = append(attachs, attach.([]slack.Attachment)...)
	}

	iconURL := config.GetIconURL(&msg, b.GetString(iconURLConfig))
	matterMessage := matterhook.OMessage{
		IconURL:     iconURL,
		Attachments: attachs,
		Channel:     msg.Channel,
		UserName:    msg.Username,
		Text:        msg.Text,
	}
	if msg.Avatar != "" {
		matterMessage.IconURL = msg.Avatar
	}
	if err := b.mh.Send(matterMessage); err != nil {
		b.Log.Errorf("Failed to send message via webhook: %#v", err)
		return err
	}
	return nil
}

func (b *Bslack) sendAPI(msg config.Message) (string, error) {
	// Handle channelmember messages.
	if handled := b.handleGetChannelMembers(&msg); handled {
		return "", nil
	}

	channelInfo, err := b.channels.getChannel(msg.Channel)
	if err != nil {
		return "", fmt.Errorf("could not send message: %v", err)
	}
	if msg.Event == config.EventUserTyping {
		if b.GetBool("ShowUserTyping") {
			b.Log.Debug("User typing event received (not supported by Slack Web API)")
		}
		return "", nil
	}

	var handled bool

	// Handle topic/purpose updates.
	if handled, err = b.handleTopicOrPurpose(&msg, channelInfo); handled {
		return "", err
	}

	// Handle prefix hint for unthreaded messages.
	if msg.ParentNotFound() {
		msg.ParentID = ""
		msg.Text = fmt.Sprintf("[thread]: %s", msg.Text)
	}

	// Handle message deletions.
	if handled, err = b.deleteMessage(&msg, channelInfo); handled {
		return msg.ID, err
	}

	// Prepend nickname if configured.
	if b.GetBool(useNickPrefixConfig) {
		msg.Text = msg.Username + msg.Text
	}

	// Handle message edits.
	if handled, err = b.editMessage(&msg, channelInfo); handled {
		return msg.ID, err
	}

	// Upload a file if it exists.
	if len(msg.Extra) > 0 {
		extraMsgs := helper.HandleExtra(&msg, b.General)
		for i := range extraMsgs {
			rmsg := &extraMsgs[i]
			rmsg.Text = rmsg.Username + rmsg.Text
			_, err = b.postAPIMessage(rmsg, channelInfo)
			if err != nil {
				b.Log.Error(err)
			}
		}
		// Upload files if necessary (from Slack, Telegram or Mattermost).
		return b.uploadFile(&msg, channelInfo.ID)
	}

	// Post message.
	return b.postAPIMessage(&msg, channelInfo)
}

func (b *Bslack) updateTopicOrPurpose(msg *config.Message, channelInfo *slack.Channel) error {
	incomingChangeType, text := b.extractTopicOrPurpose(msg.Text)

	for {
		var err error
		switch incomingChangeType {
		case "topic":
			_, err = b.sc.SetTopicOfConversation(channelInfo.ID, text)
		case "purpose":
			_, err = b.sc.SetPurposeOfConversation(channelInfo.ID, text)
		default:
			b.Log.Errorf("Unhandled type received from extractTopicOrPurpose: %s", incomingChangeType)
			return nil
		}

		if err == nil {
			return nil
		}

		if err = handleRateLimit(b.Log, err); err != nil {
			return err
		}
	}
}

// handles updating topic/purpose and determining whether to further propagate update messages.
func (b *Bslack) handleTopicOrPurpose(msg *config.Message, channelInfo *slack.Channel) (bool, error) {
	if msg.Event != config.EventTopicChange {
		return false, nil
	}

	if b.GetBool("SyncTopic") {
		return true, b.updateTopicOrPurpose(msg, channelInfo)
	}

	// Pass along to normal message handlers.
	if b.GetBool("ShowTopicChange") {
		return false, nil
	}

	// Swallow message as handled no-op.
	return true, nil
}

func (b *Bslack) deleteMessage(msg *config.Message, channelInfo *slack.Channel) (bool, error) {
	if msg.Event != config.EventMsgDelete {
		return false, nil
	}

	// Some protocols echo deletes, but with an empty ID.
	if msg.ID == "" {
		return true, nil
	}

	for {
		_, _, err := b.sc.DeleteMessage(channelInfo.ID, msg.ID)
		if err == nil {
			return true, nil
		}

		if err = handleRateLimit(b.Log, err); err != nil {
			b.Log.Errorf("Failed to delete user message from Slack: %#v", err)
			return true, err
		}
	}
}

func (b *Bslack) editMessage(msg *config.Message, channelInfo *slack.Channel) (bool, error) {
	if msg.ID == "" {
		return false, nil
	}
	messageOptions := b.prepareMessageOptions(msg)
	for {
		_, _, _, err := b.sc.UpdateMessage(channelInfo.ID, msg.ID, messageOptions...)
		if err == nil {
			return true, nil
		}

		if err = handleRateLimit(b.Log, err); err != nil {
			b.Log.Errorf("Failed to edit user message on Slack: %#v", err)
			return true, err
		}
	}
}

func (b *Bslack) postAPIMessage(msg *config.Message, channelInfo *slack.Channel) (string, error) {

	// don't post empty messages
	if msg.Text == "" {
		return "", nil
	}
	messageOptions := b.prepareMessageOptions(msg)
	for {
		_, id, err := b.sc.PostMessage(channelInfo.ID, messageOptions...)
		if err == nil {
			return id, nil
		}

		if err = handleRateLimit(b.Log, err); err != nil {
			b.Log.Errorf("Failed to sent user message to Slack: %#v", err)
			return "", err
		}
	}
}

// uploadFile handles native upload of files
func (b *Bslack) uploadFile(msg *config.Message, channelID string) (string, error) {
	var messageID string
	for _, f := range msg.Extra["file"] {
		fi, ok := f.(config.FileInfo)
		if !ok {
			b.Log.Errorf("Received a file with unexpected content: %#v", f)
			continue
		}
		if msg.Text == fi.Comment {
			msg.Text = ""
		}
		// Because the result of the UploadFile is slower than the MessageEvent from slack
		// we can't match on the file ID yet, so we have to match on the filename too.
		ts := time.Now()
		b.Log.Debugf("Adding file %s to cache at %s with timestamp", fi.Name, ts.String())
		b.cache.Add("filename"+fi.Name, ts)
		initialComment := fmt.Sprintf("File from %s", msg.Username)
		if fi.Comment != "" {
			initialComment += fmt.Sprintf(" with comment: %s", fi.Comment)
		}
		res, err := b.sc.UploadFile(slack.FileUploadParameters{
			Reader:          bytes.NewReader(*fi.Data),
			Filename:        fi.Name,
			Channels:        []string{channelID},
			InitialComment:  initialComment,
			ThreadTimestamp: msg.ParentID,
		})
		if err != nil {
			b.Log.Errorf("uploadfile %#v", err)
			return "", err
		}
		if res.ID != "" {
			b.Log.Debugf("Adding file ID %s to cache with timestamp %s", res.ID, ts.String())
			b.cache.Add("file"+res.ID, ts)

			// search for message id by uploaded file in private/public channels, get thread timestamp from uploaded file
			if v, ok := res.Shares.Private[channelID]; ok && len(v) > 0 {
				messageID = v[0].Ts
			}
			if v, ok := res.Shares.Public[channelID]; ok && len(v) > 0 {
				messageID = v[0].Ts
			}
		}
	}
	return messageID, nil
}

func (b *Bslack) prepareMessageOptions(msg *config.Message) []slack.MsgOption {
	params := slack.NewPostMessageParameters()
	if b.GetBool(useNickPrefixConfig) {
		params.AsUser = true
	}
	params.Username = msg.Username
	params.LinkNames = 1 // replace mentions
	params.IconURL = config.GetIconURL(msg, b.GetString(iconURLConfig))
	params.ThreadTimestamp = msg.ParentID
	if msg.Avatar != "" {
		params.IconURL = msg.Avatar
	}

	var attachments []slack.Attachment
	// add file attachments
	attachments = append(attachments, b.createAttach(msg.Extra)...)
	// add slack attachments (from another slack bridge)
	if msg.Extra != nil {
		for _, attach := range msg.Extra[sSlackAttachment] {
			attachments = append(attachments, attach.([]slack.Attachment)...)
		}
	}

	var opts []slack.MsgOption
	opts = append(opts,
		// provide regular text field (fallback used in Slack notifications, etc.)
		slack.MsgOptionText(msg.Text, false),

		// add a callback ID so we can see we created it
		slack.MsgOptionBlocks(slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, msg.Text, false, false),
			nil, nil,
			slack.SectionBlockOptionBlockID("matterbridge_"+b.uuid),
		)),

		slack.MsgOptionEnableLinkUnfurl(),
	)
	opts = append(opts, slack.MsgOptionAttachments(attachments...))
	opts = append(opts, slack.MsgOptionPostMessageParameters(params))
	return opts
}

func (b *Bslack) createAttach(extra map[string][]interface{}) []slack.Attachment {
	var attachements []slack.Attachment
	for _, v := range extra["attachments"] {
		entry := v.(map[string]interface{})
		s := slack.Attachment{
			Fallback:   extractStringField(entry, "fallback"),
			Color:      extractStringField(entry, "color"),
			Pretext:    extractStringField(entry, "pretext"),
			AuthorName: extractStringField(entry, "author_name"),
			AuthorLink: extractStringField(entry, "author_link"),
			AuthorIcon: extractStringField(entry, "author_icon"),
			Title:      extractStringField(entry, "title"),
			TitleLink:  extractStringField(entry, "title_link"),
			Text:       extractStringField(entry, "text"),
			ImageURL:   extractStringField(entry, "image_url"),
			ThumbURL:   extractStringField(entry, "thumb_url"),
			Footer:     extractStringField(entry, "footer"),
			FooterIcon: extractStringField(entry, "footer_icon"),
		}
		attachements = append(attachements, s)
	}
	return attachements
}

func extractStringField(data map[string]interface{}, field string) string {
	if rawValue, found := data[field]; found {
		if value, ok := rawValue.(string); ok {
			return value
		}
	}
	return ""
}

func (b *Bslack) handleSlackEvents(w http.ResponseWriter, r *http.Request) {
	var evt SlackEventWrapper
	body, _ := io.ReadAll(r.Body)
	b.Log.Infof("Received raw Slack event: %s", string(body))
	_ = json.Unmarshal(body, &evt)

	if evt.Type == "url_verification" {
		var challenge struct {
			Challenge string `json:"challenge"`
		}
		if err := json.Unmarshal(body, &challenge); err != nil {
			b.Log.Errorf("Could not parse challenge: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]string{"challenge": challenge.Challenge}
		json.NewEncoder(w).Encode(resp)
		return
	}

	if evt.Type == "event_callback" && evt.Event.Type == "message" && evt.Event.SubType != "bot_message" {
		channel, err := b.channels.getChannelByID(evt.Event.Channel)
		if err != nil {
			b.Log.Errorf("Could not get channel name for ID %s: %v", evt.Event.Channel, err)
			return
		}
		username := evt.Event.User // fallback to ID if needed
		if evt.Event.User != "" {
			userInfo, err := b.sc.GetUserInfo(evt.Event.User)
			if err != nil {
				b.Log.Warnf("Could not fetch username for user ID %s: %v", evt.Event.User, err)
			} else if userInfo.Profile.DisplayName != "" {
				username = userInfo.Profile.DisplayName
			}
		}

		msg := config.Message{
			Text:     evt.Event.Text,
			Channel:  channel.Name,
			Username: username,
			Account:  b.Account,
			Protocol: "slack",
		}
		b.Log.Infof("Relaying Slack message from user %s in channel %s", msg.Username, channel.Name)
		b.Remote <- msg
	}

	w.WriteHeader(http.StatusOK)
}

type SlackEventWrapper struct {
	Type      string `json:"type"`
	Challenge string `json:"challenge,omitempty"`
	Event     struct {
		Type            string `json:"type"`
		SubType         string `json:"subtype,omitempty"` // <--- Add this
		User            string `json:"user,omitempty"`
		Text            string `json:"text,omitempty"`
		Channel         string `json:"channel"`
		Ts              string `json:"ts"`
		DeletedTs       string `json:"deleted_ts,omitempty"` // <--- Add this
		PreviousMessage struct {
			Ts   string `json:"ts"`
			User string `json:"user"`
			Text string `json:"text"`
		} `json:"previous_message,omitempty"`
	} `json:"event"`
}
