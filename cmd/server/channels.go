package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/sahand363/sahand-cortext/internal/channel"
)

// Channels: AI-written posts for the owner's Telegram channels, drafted in
// the hub, approved in the hub or by buttons in a Telegram DM, then posted.
//
//	GET    /api/channels                      list
//	POST   /api/channels                      create {name, chat_ref, persona_id, bot_token, instructions}
//	GET    /api/channels/{id}                 one channel + readiness check
//	PUT    /api/channels/{id}                 update (bot_token "" keeps the old one)
//	DELETE /api/channels/{id}                 delete channel and its drafts
//	POST   /api/channels/{id}/learn           rebuild style {samples: []string}
//	POST   /api/channels/{id}/unpair          forget the paired Telegram owner
//	GET    /api/channels/{id}/posts           drafts and posts, newest first
//	POST   /api/channels/{id}/posts           write a draft {idea, notes}
//	PUT    /api/channel-posts/{pid}           save edited text {draft}
//	POST   /api/channel-posts/{pid}/{action}  regenerate {feedback} | approve | publish | discard | unpublish
//	DELETE /api/channel-posts/{pid}           delete a draft from the hub

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(v)
}

func channelErr(w http.ResponseWriter, err error) {
	status := 400
	if errors.Is(err, channel.ErrNotFound) {
		status = 404
	}
	jsonOut(w, status, map[string]string{"error": err.Error()})
}

type channelInput struct {
	Name         string  `json:"name"`
	ChatRef      string  `json:"chat_ref"`
	PersonaID    string  `json:"persona_id"`
	BotToken     string  `json:"bot_token"`
	Instructions string  `json:"instructions"`
	StyleGuide   *string `json:"style_guide"`
	Enabled      *bool   `json:"enabled"`
}

// validateChannel fills in the bot username and refuses a token that a
// persona's chat bot already polls (Telegram allows one poller per token).
func (a *App) validateChannel(ctx context.Context, c *channel.Channel, token string) error {
	c.ChatRef = channel.NormalizeChatRef(c.ChatRef)
	if c.ChatRef == "" {
		return errors.New("enter the channel link or @username")
	}
	if strings.TrimSpace(c.Name) == "" {
		c.Name = c.ChatRef
	}
	if token = strings.TrimSpace(token); token != "" {
		if who := a.tokenUsedByPersona(ctx, token); who != "" {
			return errors.New("this bot already runs the persona \"" + who + "\". Create a separate bot in @BotFather for the channel")
		}
		chans, _ := a.channelStore().List(ctx)
		for _, other := range chans {
			if other.ID != c.ID && other.HasToken && a.channels.SameToken(other, token) {
				return errors.New("this bot already runs the channel \"" + other.Name + "\"")
			}
		}
		name, err := a.channels.BotUsername(ctx, token)
		if err != nil {
			return errors.New("that bot token doesn't work: " + err.Error())
		}
		c.BotUsername = name
	}
	return nil
}

func (a *App) tokenUsedByPersona(ctx context.Context, token string) string {
	list, err := a.personaStore.List(ctx, true)
	if err != nil {
		return ""
	}
	for _, p := range list {
		if strings.TrimSpace(p.TelegramBotToken) == token {
			return p.Name
		}
	}
	return ""
}

func (a *App) channelStore() *channel.Store { return a.channels.Store }

func (a *App) handleChannels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	switch r.Method {
	case http.MethodGet:
		list, err := a.channelStore().List(ctx)
		if err != nil {
			channelErr(w, err)
			return
		}
		if list == nil {
			list = []*channel.Channel{}
		}
		jsonOut(w, 200, map[string]any{"channels": list})
	case http.MethodPost:
		var in channelInput
		if err := readJSON(r, &in); err != nil {
			channelErr(w, errors.New("invalid request"))
			return
		}
		c := &channel.Channel{Name: strings.TrimSpace(in.Name), ChatRef: in.ChatRef, PersonaID: in.PersonaID, Instructions: strings.TrimSpace(in.Instructions), Enabled: true, Examples: []string{}}
		if strings.TrimSpace(in.BotToken) == "" {
			channelErr(w, errors.New("paste the bot token from @BotFather"))
			return
		}
		if err := a.validateChannel(ctx, c, in.BotToken); err != nil {
			channelErr(w, err)
			return
		}
		if err := a.channelStore().Save(ctx, c, in.BotToken); err != nil {
			channelErr(w, err)
			return
		}
		a.channels.Kick()
		c, _ = a.channelStore().Get(ctx, c.ID)
		jsonOut(w, 200, map[string]any{"channel": c})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (a *App) handleChannelByID(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/channels/"), "/")
	parts := strings.Split(rest, "/")
	id := parts[0]
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}
	c, err := a.channelStore().Get(ctx, id)
	if err != nil {
		channelErr(w, err)
		return
	}
	switch {
	case sub == "" && r.Method == http.MethodGet:
		cctx, cancel := context.WithTimeout(ctx, 12*time.Second)
		defer cancel()
		jsonOut(w, 200, map[string]any{"channel": c, "check": a.channels.Check(cctx, c)})

	case sub == "" && r.Method == http.MethodPut:
		var in channelInput
		if err := readJSON(r, &in); err != nil {
			channelErr(w, errors.New("invalid request"))
			return
		}
		c.Name, c.ChatRef, c.PersonaID, c.Instructions = strings.TrimSpace(in.Name), in.ChatRef, in.PersonaID, strings.TrimSpace(in.Instructions)
		if in.StyleGuide != nil {
			c.StyleGuide = strings.TrimSpace(*in.StyleGuide)
		}
		if in.Enabled != nil {
			c.Enabled = *in.Enabled
		}
		if err := a.validateChannel(ctx, c, in.BotToken); err != nil {
			channelErr(w, err)
			return
		}
		if err := a.channelStore().Save(ctx, c, in.BotToken); err != nil {
			channelErr(w, err)
			return
		}
		a.channels.Kick()
		c, _ = a.channelStore().Get(ctx, c.ID)
		jsonOut(w, 200, map[string]any{"channel": c})

	case sub == "" && r.Method == http.MethodDelete:
		if err := a.channelStore().Delete(ctx, id); err != nil {
			channelErr(w, err)
			return
		}
		a.channels.Kick()
		jsonOut(w, 200, map[string]any{"ok": true})

	case sub == "learn" && r.Method == http.MethodPost:
		var in struct {
			Samples []string `json:"samples"`
		}
		_ = readJSON(r, &in)
		lctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
		defer cancel()
		c, n, err := a.channels.LearnStyle(lctx, id, in.Samples)
		if err != nil {
			channelErr(w, err)
			return
		}
		jsonOut(w, 200, map[string]any{"channel": c, "learned_from": n})

	case sub == "unpair" && r.Method == http.MethodPost:
		if err := a.channelStore().ResetOwner(ctx, id); err != nil {
			channelErr(w, err)
			return
		}
		c, _ = a.channelStore().Get(ctx, id)
		jsonOut(w, 200, map[string]any{"channel": c})

	case sub == "posts" && r.Method == http.MethodGet:
		posts, err := a.channelStore().Posts(ctx, id, 100)
		if err != nil {
			channelErr(w, err)
			return
		}
		if posts == nil {
			posts = []*channel.Post{}
		}
		jsonOut(w, 200, map[string]any{"posts": posts})

	case sub == "posts" && r.Method == http.MethodPost:
		var in struct {
			Idea  string `json:"idea"`
			Notes string `json:"notes"`
		}
		if err := readJSON(r, &in); err != nil {
			channelErr(w, errors.New("invalid request"))
			return
		}
		gctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
		defer cancel()
		p, err := a.channels.Generate(gctx, id, in.Idea, in.Notes)
		if err != nil {
			channelErr(w, err)
			return
		}
		jsonOut(w, 200, map[string]any{"post": p})

	default:
		http.Error(w, "not found", 404)
	}
}

func (a *App) handleChannelPost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/channel-posts/"), "/")
	parts := strings.Split(rest, "/")
	pid := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	var (
		p   *channel.Post
		err error
	)
	switch {
	case action == "" && r.Method == http.MethodPut:
		var in struct {
			Draft string `json:"draft"`
		}
		if err := readJSON(r, &in); err != nil {
			channelErr(w, errors.New("invalid request"))
			return
		}
		p, err = a.channels.EditDraft(ctx, pid, in.Draft)
	case action == "" && r.Method == http.MethodDelete:
		if p, err = a.channelStore().GetPost(ctx, pid); err == nil {
			if p.Status == channel.StatusPending {
				_, _ = a.channels.Discard(ctx, pid)
			}
			err = a.channelStore().DeletePost(ctx, pid)
		}
		if err != nil {
			channelErr(w, err)
			return
		}
		jsonOut(w, 200, map[string]any{"ok": true})
		return
	case r.Method == http.MethodPost:
		actx, cancel := context.WithTimeout(ctx, 4*time.Minute)
		defer cancel()
		switch action {
		case "regenerate":
			var in struct {
				Feedback string `json:"feedback"`
			}
			_ = readJSON(r, &in)
			p, err = a.channels.Regenerate(actx, pid, in.Feedback)
		case "approve":
			p, err = a.channels.SendForApproval(actx, pid)
		case "publish":
			p, err = a.channels.Publish(actx, pid)
		case "discard":
			p, err = a.channels.Discard(actx, pid)
		case "unpublish":
			p, err = a.channels.Unpublish(actx, pid)
		default:
			http.Error(w, "not found", 404)
			return
		}
	default:
		http.Error(w, "method not allowed", 405)
		return
	}
	if err != nil {
		channelErr(w, err)
		return
	}
	jsonOut(w, 200, map[string]any{"post": p})
}
