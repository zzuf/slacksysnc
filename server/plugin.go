package main

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/mattermost/mattermost-server/v6/model"
	"github.com/mattermost/mattermost-server/v6/plugin"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

// Plugin implements the interface expected by the Mattermost server to communicate between the server and plugin processes.
type Plugin struct {
	plugin.MattermostPlugin

	// configurationLock synchronizes access to the configuration.
	configurationLock sync.RWMutex

	// configuration is the active plugin configuration. Consult getConfiguration and
	// setConfiguration for usage.
	configuration *configuration
}

var api = slack.New(os.Getenv("SLACK_TOKEN"))
var signingSecret = os.Getenv("SLACK_SIGNING_SECRET")
var teamID = "6rjwrkb71jyn9cbdo5z7nu4rja"

//id:name
var slackUserMap = map[string]string{}
var slackChannelMap = map[string]string{}

//username:userObject
var mmUserMap = map[string]*model.User{}
var mmChannelMap = map[string]*model.Channel{}

func getSlackUserFromId(id string) string {
	var n string
	n, ok := slackUserMap[id]
	if ok != true {
		u, err := api.GetUserInfo(id)
		if err != nil {
			fmt.Println(err)
		}
		n = u.Name
		slackUserMap[id] = u.Name
	}
	return n
}

func getSlackChannelFromId(id string) string {
	var n string
	n, ok := slackChannelMap[id]
	if ok != true {
		c, err := api.GetConversationInfo(id, false)
		if err != nil {
			fmt.Println(err)
		}
		n = c.Name
		slackChannelMap[id] = c.Name
	}
	return n
}

func (p *Plugin) getMMUserFromName(name string) *model.User {
	var u *model.User
	u, ok := mmUserMap[name]
	if ok != true {
		users, _ := p.API.GetUsersByUsernames([]string{name})
		mmUserMap[name] = users[0]
		u = users[0]
	}
	return u
}

func (p *Plugin) createMMChannel(name string) *model.Channel {
	var channel = &model.Channel{DisplayName: name, Name: sanitizeMMChannelName(name), Type: "O", TeamId: teamID}
	c, err := p.API.CreateChannel(channel)
	if c == nil {
		fmt.Println(err)
	}
	return c
}

func sanitizeMMChannelName(name string) string {
	r := regexp.MustCompile(`[a-z0-9\-_]+`)
	if r.MatchString(name) {
		return name
	} else {
		md5 := md5.Sum([]byte(name))
		return fmt.Sprintf("%x", md5)
	}
}

func (p *Plugin) getMMChannelFromName(name string) *model.Channel {
	var c *model.Channel
	name = sanitizeMMChannelName(name)
	c, ok := mmChannelMap[name]
	if ok != true {
		channel, err := p.API.GetChannelByName(teamID, name, false)
		if channel == nil {
			fmt.Println(err)
			channel = p.createMMChannel(name)
			fmt.Println(channel.Name)
		}
		mmChannelMap[name] = channel
		c = channel
	}
	return c
}

func (p *Plugin) userIDConvert(id string) *model.User {
	slackName := getSlackUserFromId(id)
	return p.getMMUserFromName(slackName)
}

func (p *Plugin) channelIDConvert(id string) *model.Channel {
	slackChannelName := getSlackChannelFromId(id)
	return p.getMMChannelFromName(slackChannelName)
}

func convertMention(m string) string {
	regexp := regexp.MustCompile("<@[A-Z0-9]+>")
	mentions := regexp.FindAllString(m, -1)
	for _, mention := range mentions {
		username := getSlackUserFromId(mention[2 : len(mention)-1])
		m = strings.Replace(m, mention, "@"+username, 1)
	}
	return m
}

// ServeHTTP demonstrates a plugin that handles HTTP requests by greeting the world.
func (p *Plugin) ServeHTTP(c *plugin.Context, w http.ResponseWriter, r *http.Request) {
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	sv, err := slack.NewSecretsVerifier(r.Header, signingSecret)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if _, err := sv.Write(body); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if err := sv.Ensure(); err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	eventsAPIEvent, err := slackevents.ParseEvent(json.RawMessage(body), slackevents.OptionNoVerifyToken())
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if eventsAPIEvent.Type == slackevents.URLVerification {
		var r *slackevents.ChallengeResponse
		err := json.Unmarshal([]byte(body), &r)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text")
		w.Write([]byte(r.Challenge))
	}
	if eventsAPIEvent.Type == slackevents.CallbackEvent {
		innerEvent := eventsAPIEvent.InnerEvent
		switch ev := innerEvent.Data.(type) {
		case *slackevents.ChannelCreatedEvent:
			println(ev.Channel.Name)
			c, _, _, err := api.JoinConversation(ev.Channel.ID)
			if err != nil {
				fmt.Println(err)
			}
			p.createMMChannel(c.Name)
		case *slackevents.MessageEvent:
			if ev.ChannelType == "channel" && ev.Edited == nil && ev.Text != "" {
				uID := p.userIDConvert(ev.User).Id
				cID := p.channelIDConvert(ev.Channel).Id
				message := convertMention(ev.Text)
				var post *model.Post
				createat, err := strconv.ParseInt(strings.Replace(ev.TimeStamp, ".", "", 1)[:13], 10, 64)
				if err != nil {
					p.API.LogDebug("ParseInt error.")
				}
				if ev.ThreadTimeStamp != "" {
					treadTimeStamp, err := strconv.ParseInt(strings.Replace(ev.ThreadTimeStamp, ".", "", 1)[:13], 10, 64)
					if err != nil {
						p.API.LogDebug("ParseInt error.")
					}
					posts, err := p.API.GetPostsSince(cID, treadTimeStamp-100)
					threadID := posts.Order[len(posts.Order)-1]
					post = &model.Post{DeleteAt: 0, EditAt: 0, Hashtags: "", ChannelId: cID, Message: message, UserId: uID, CreateAt: createat, RootId: threadID}
				} else {
					post = &model.Post{DeleteAt: 0, EditAt: 0, Hashtags: "", ChannelId: cID, Message: message, UserId: uID, CreateAt: createat}
				}
				post, _ = p.API.CreatePost(post)
			}
		}
	}
}
