package bslack

import (
	"errors"
	"fmt"
	"html"
	"time"

	"github.com/42wim/matterbridge/bridge/config"
	"github.com/42wim/matterbridge/bridge/helper"
	"github.com/slack-go/slack"
)

// ErrEventIgnored is for events that should be ignored
var ErrEventIgnored = errors.New("this event message should ignored")

func (b *Bslack) handleSlack() {
	messages := make(chan *config.Message)
	if b.GetString(incomingWebhookConfig) != "" && b.GetString(tokenConfig) == "" {
		b.Log.Debugf("Choosing webhooks based receiving")
		go b.handleMatterHook(messages)
	} else {
		b.Log.Debugf("Slack is using the Web API + Events. Messages will be received via /slack/events")
	}
	time.Sleep(time.Second)
	b.Log.Debug("Start listening for Slack messages")
	for message := range messages {
		// don't do any action on deleted/typing messages
		if message.Event != config.EventUserTyping && message.Event != config.EventMsgDelete &&
			message.Event != config.EventFileDelete {
			b.Log.Debugf("<= Sending message from %s on %s to gateway", message.Username, b.Account)
			// cleanup the message
			message.Text = b.replaceMention(message.Text)
			message.Text = b.replaceVariable(message.Text)
			message.Text = b.replaceChannel(message.Text)
			message.Text = b.replaceURL(message.Text)
			message.Text = b.replaceb0rkedMarkDown(message.Text)
			message.Text = html.UnescapeString(message.Text)

			// Add the avatar
			message.Avatar = b.users.getAvatar(message.UserID)
		}

		b.Log.Debugf("<= Message is %#v", message)
		b.Remote <- *message
	}
}


func (b *Bslack) handleMatterHook(messages chan *config.Message) {
	for {
		message := b.mh.Receive()
		b.Log.Debugf("receiving from matterhook (slack) %#v", message)
		if message.UserName == "slackbot" {
			continue
		}
		messages <- &config.Message{
			Username: message.UserName,
			Text:     message.Text,
			Channel:  message.ChannelName,
		}
	}
}

// skipMessageEvent skips event that need to be skipped :-)
func (b *Bslack) skipMessageEvent(ev *slack.MessageEvent) bool {
	switch ev.SubType {
	case sChannelLeave, sChannelJoin:
		return b.GetBool(noSendJoinConfig)
	case sPinnedItem, sUnpinnedItem:
		return true
	case sChannelTopic, sChannelPurpose:
		// Skip the event if our bot/user account changed the topic/purpose
		if ev.User == "" || ev.User == sSlackBotUser {
			return true
		}
	}

	// Check for our callback ID
	hasOurCallbackID := false
	if len(ev.Blocks.BlockSet) == 1 {
		block, ok := ev.Blocks.BlockSet[0].(*slack.SectionBlock)
		hasOurCallbackID = ok && block.BlockID == "matterbridge_"+b.uuid
	}

	if ev.SubMessage != nil {
		// It seems ev.SubMessage.Edited == nil when slack unfurls.
		// Do not forward these messages. See Github issue #266.
		if ev.SubMessage.ThreadTimestamp != ev.SubMessage.Timestamp &&
			ev.SubMessage.Edited == nil {
			return true
		}
		// see hidden subtypes at https://api.slack.com/events/message
		// these messages are sent when we add a message to a thread #709
		if ev.SubType == "message_replied" && ev.Hidden {
			return true
		}
		if len(ev.SubMessage.Blocks.BlockSet) == 1 {
			block, ok := ev.SubMessage.Blocks.BlockSet[0].(*slack.SectionBlock)
			hasOurCallbackID = ok && block.BlockID == "matterbridge_"+b.uuid
		}
	}

	// Skip any messages that we made ourselves or from 'slackbot' (see #527).
	if ev.Username == sSlackBotUser || hasOurCallbackID {
		return true
	}

	if len(ev.Files) > 0 {
		return b.filesCached(ev.Files)
	}
	return false
}

func (b *Bslack) filesCached(files []slack.File) bool {
	for i := range files {
		if !b.fileCached(&files[i]) {
			return false
		}
	}
	return true
}

// handleMessageEvent handles the message events. Together with any called sub-methods,
// this method implements the following event processing pipeline:
//
// 1. Check if the message should be ignored.
//    NOTE: This is not actually part of the method below but is done just before it
//          is called via the 'skipMessageEvent()' method.
// 2. Populate the Matterbridge message that will be sent to the router based on the
//    received event and logic that is common to all events that are not skipped.
// 3. Detect and handle any message that is "status" related (think join channel, etc.).
//    This might result in an early exit from the pipeline and passing of the
//    pre-populated message to the Matterbridge router.
// 4. Handle the specific case of messages that edit existing messages depending on
//    configuration.
// 5. Handle any attachments of the received event.
// 6. Check that the Matterbridge message that we end up with after at the end of the
//    pipeline is valid before sending it to the Matterbridge router.
func (b *Bslack) handleMessageEvent(ev *slack.MessageEvent) (*config.Message, error) {
	rmsg, err := b.populateReceivedMessage(ev)
	if err != nil {
		return nil, err
	}

	// Handle some message types early.
	if b.handleStatusEvent(ev, rmsg) {
		return rmsg, nil
	}

	b.handleAttachments(ev, rmsg)

	// Verify that we have the right information and the message
	// is well-formed before sending it out to the router.
	if len(ev.Files) == 0 && (rmsg.Text == "" || rmsg.Username == "") {
		if ev.BotID != "" {
			// This is probably a webhook we couldn't resolve.
			return nil, fmt.Errorf("message handling resulted in an empty bot message (probably an incoming webhook we couldn't resolve): %#v", ev)
		}
		if ev.SubMessage != nil {
			return nil, fmt.Errorf("message handling resulted in an empty message: %#v with submessage %#v", ev, ev.SubMessage)
		}
		return nil, fmt.Errorf("message handling resulted in an empty message: %#v", ev)
	}
	return rmsg, nil
}

func (b *Bslack) handleFileDeletedEvent(ev *slack.FileDeletedEvent) (*config.Message, error) {
	if rawChannel, ok := b.cache.Get(cfileDownloadChannel + ev.FileID); ok {
		channel, err := b.channels.getChannelByID(rawChannel.(string))
		if err != nil {
			return nil, err
		}

		return &config.Message{
			Event:    config.EventFileDelete,
			Text:     config.EventFileDelete,
			Channel:  channel.Name,
			Account:  b.Account,
			ID:       ev.FileID,
			Protocol: b.Protocol,
		}, nil
	}

	return nil, fmt.Errorf("channel ID for file ID %s not found", ev.FileID)
}

func (b *Bslack) handleStatusEvent(ev *slack.MessageEvent, rmsg *config.Message) bool {
	switch ev.SubType {
	case sChannelJoined, sMemberJoined:
		// There's no further processing needed on channel events
		// so we return 'true'.
		return true
	case sChannelJoin, sChannelLeave:
		rmsg.Username = sSystemUser
		rmsg.Event = config.EventJoinLeave
	case sChannelTopic, sChannelPurpose:
		b.channels.populateChannels(false)
		rmsg.Event = config.EventTopicChange
	case sMessageChanged:
		rmsg.Text = ev.SubMessage.Text
		// handle deleted thread starting messages
		if ev.SubMessage.Text == "This message was deleted." {
			rmsg.Event = config.EventMsgDelete
			return true
		}
	case sMessageDeleted:
		rmsg.Text = config.EventMsgDelete
		rmsg.Event = config.EventMsgDelete
		rmsg.ID = ev.DeletedTimestamp
		// If a message is being deleted we do not need to process
		// the event any further so we return 'true'.
		return true
	case sMeMessage:
		rmsg.Event = config.EventUserAction
	}
	return false
}

func getMessageTitle(attach *slack.Attachment) string {
	if attach.TitleLink != "" {
		return fmt.Sprintf("[%s](%s)\n", attach.Title, attach.TitleLink)
	}
	return attach.Title
}

func (b *Bslack) handleAttachments(ev *slack.MessageEvent, rmsg *config.Message) {
	// File comments are set by the system (because there is no username given).
	if ev.SubType == sFileComment {
		rmsg.Username = sSystemUser
	}

	// See if we have some text in the attachments.
	if rmsg.Text == "" {
		for i, attach := range ev.Attachments {
			if attach.Text != "" {
				if attach.Title != "" {
					rmsg.Text = getMessageTitle(&ev.Attachments[i])
				}
				rmsg.Text += attach.Text
				if attach.Footer != "" {
					rmsg.Text += "\n\n" + attach.Footer
				}
			} else {
				rmsg.Text = attach.Fallback
			}
		}
	}

	// Save the attachments, so that we can send them to other slack (compatible) bridges.
	if len(ev.Attachments) > 0 {
		rmsg.Extra[sSlackAttachment] = append(rmsg.Extra[sSlackAttachment], ev.Attachments)
	}

	// If we have files attached, download them (in memory) and put a pointer to it in msg.Extra.
	for i := range ev.Files {
		// keep reference in cache on which channel we added this file
		b.cache.Add(cfileDownloadChannel+ev.Files[i].ID, ev.Channel)
		if err := b.handleDownloadFile(rmsg, &ev.Files[i], false); err != nil {
			b.Log.Errorf("Could not download incoming file: %#v", err)
		}
	}
}

func (b *Bslack) handleTypingEvent(ev *slack.UserTypingEvent) (*config.Message, error) {
	channelInfo, err := b.channels.getChannelByID(ev.Channel)
	if err != nil {
		return nil, err
	}
	return &config.Message{
		Channel: channelInfo.Name,
		Account: b.Account,
		Event:   config.EventUserTyping,
	}, nil
}

// handleDownloadFile handles file download
func (b *Bslack) handleDownloadFile(rmsg *config.Message, file *slack.File, retry bool) error {
	if b.fileCached(file) {
		return nil
	}
	// Check that the file is neither too large nor blacklisted.
	if err := helper.HandleDownloadSize(b.Log, rmsg, file.Name, int64(file.Size), b.General); err != nil {
		b.Log.WithError(err).Infof("Skipping download of incoming file.")
		return nil
	}

	// Actually download the file.
	data, err := helper.DownloadFileAuth(file.URLPrivateDownload, "Bearer "+b.GetString(tokenConfig))
	if err != nil {
		return fmt.Errorf("download %s failed %#v", file.URLPrivateDownload, err)
	}

	if len(*data) != file.Size && !retry {
		b.Log.Debugf("Data size (%d) is not equal to size declared (%d)\n", len(*data), file.Size)
		time.Sleep(1 * time.Second)
		return b.handleDownloadFile(rmsg, file, true)
	}

	// If a comment is attached to the file(s) it is in the 'Text' field of the Slack messge event
	// and should be added as comment to only one of the files. We reset the 'Text' field to ensure
	// that the comment is not duplicated.
	comment := rmsg.Text
	rmsg.Text = ""
	helper.HandleDownloadData2(b.Log, rmsg, file.Name, file.ID, comment, file.URLPrivateDownload, data, b.General)
	return nil
}

// handleGetChannelMembers handles messages containing the GetChannelMembers event
// Sends a message to the router containing *config.ChannelMembers
func (b *Bslack) handleGetChannelMembers(rmsg *config.Message) bool {
	if rmsg.Event != config.EventGetChannelMembers {
		return false
	}

	cMembers := b.channels.getChannelMembers(b.users)

	extra := make(map[string][]interface{})
	extra[config.EventGetChannelMembers] = append(extra[config.EventGetChannelMembers], cMembers)
	msg := config.Message{
		Extra:   extra,
		Event:   config.EventGetChannelMembers,
		Account: b.Account,
	}

	b.Log.Debugf("sending msg to remote %#v", msg)
	b.Remote <- msg

	return true
}

// fileCached implements Matterbridge's caching logic for files
// shared via Slack.
//
// We consider that a file was cached if its ID was added in the last minute or
// it's name was registered in the last 10 seconds. This ensures that an
// identically named file but with different content will be uploaded correctly
// (the assumption is that such name collisions will not occur within the given
// timeframes).
func (b *Bslack) fileCached(file *slack.File) bool {
	if ts, ok := b.cache.Get("file" + file.ID); ok && time.Since(ts.(time.Time)) < time.Minute {
		return true
	} else if ts, ok = b.cache.Get("filename" + file.Name); ok && time.Since(ts.(time.Time)) < 10*time.Second {
		return true
	}
	return false
}
