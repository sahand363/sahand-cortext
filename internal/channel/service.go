package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sahand363/sahand-cortext/internal/persona"
	"github.com/sahand363/sahand-cortext/internal/provider"
)

// Resolver picks the provider and model a persona answers with.
type Resolver func(p *persona.Persona) (providerID, modelID string, err error)

type Service struct {
	Store    *Store
	personas *persona.Store
	router   *provider.Router
	keyFor   func(string) string
	resolve  Resolver
	// TokenInUse reports whether another bot (a persona's chat bot) already
	// polls this token; two pollers on one token make Telegram reject both.
	TokenInUse func(ctx context.Context, token string) string

	apiBase string // https://api.telegram.org (overridable in tests)
	client  *http.Client

	mu      sync.Mutex
	pollers map[string]*poller // keyed by channel id
	kick    chan struct{}
	genLock sync.Map // post id -> struct{}: one generation at a time
}

type poller struct {
	token  string
	cancel context.CancelFunc
}

func NewService(store *Store, personas *persona.Store, router *provider.Router, keyFor func(string) string, resolve Resolver) *Service {
	return &Service{
		Store:    store,
		personas: personas,
		router:   router,
		keyFor:   keyFor,
		resolve:  resolve,
		apiBase:  "https://api.telegram.org",
		client:   &http.Client{Timeout: 40 * time.Second},
		pollers:  map[string]*poller{},
		kick:     make(chan struct{}, 1),
	}
}

// Kick re-syncs the bot pollers now instead of within 10 seconds.
func (s *Service) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// SameToken reports whether c is configured with this bot token.
func (s *Service) SameToken(c *Channel, token string) bool {
	return c.token != "" && c.token == strings.TrimSpace(token)
}

// ---------------------------------------------------------------------------
// Telegram Bot API
// ---------------------------------------------------------------------------

type tgError struct{ desc string }

func (e *tgError) Error() string { return "telegram: " + e.desc }

func (s *Service) call(ctx context.Context, token, method string, params map[string]any, out any) error {
	body, _ := json.Marshal(params)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.apiBase+"/bot"+token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		// The URL holds the token; never let it reach logs or the UI.
		return errors.New("telegram: network error calling " + method)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var env struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return &tgError{fmt.Sprintf("bad response (HTTP %d)", resp.StatusCode)}
	}
	if !env.OK {
		return &tgError{env.Description}
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

type tgUser struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
	IsBot     bool   `json:"is_bot"`
}

func chatParam(ref string) any {
	if n, err := strconv.ParseInt(ref, 10, 64); err == nil {
		return n
	}
	return ref
}

// NormalizeChatRef turns "t.me/name", "https://t.me/s/name", "name" or
// "@name" into "@name"; numeric ids are kept.
func NormalizeChatRef(in string) string {
	r := strings.TrimSpace(in)
	if _, err := strconv.ParseInt(r, 10, 64); err == nil {
		return r
	}
	r = strings.TrimPrefix(r, "https://")
	r = strings.TrimPrefix(r, "http://")
	r = strings.TrimPrefix(r, "www.")
	r = strings.TrimPrefix(r, "telegram.me/")
	r = strings.TrimPrefix(r, "t.me/")
	r = strings.TrimPrefix(r, "s/")
	r = strings.TrimPrefix(r, "@")
	if i := strings.IndexAny(r, "/?#"); i >= 0 {
		r = r[:i]
	}
	if r == "" {
		return ""
	}
	return "@" + r
}

// publicName is the channel's username for t.me/s/<name>, or "".
func publicName(ref string) string {
	if strings.HasPrefix(ref, "@") {
		return strings.TrimPrefix(ref, "@")
	}
	return ""
}

// CheckResult says whether a channel is ready to post.
type CheckResult struct {
	BotUsername  string `json:"bot_username"`
	ChannelTitle string `json:"channel_title"`
	IsAdmin      bool   `json:"is_admin"`
	CanPost      bool   `json:"can_post"`
	OwnerPaired  bool   `json:"owner_paired"`
	Problem      string `json:"problem"`
}

func (s *Service) Check(ctx context.Context, c *Channel) CheckResult {
	var res CheckResult
	res.OwnerPaired = c.OwnerID != 0
	if c.token == "" {
		res.Problem = "Add the bot token from @BotFather."
		return res
	}
	var me tgUser
	if err := s.call(ctx, c.token, "getMe", nil, &me); err != nil {
		res.Problem = "The bot token doesn't work: " + err.Error()
		return res
	}
	res.BotUsername = me.Username
	var chat struct {
		Title string `json:"title"`
	}
	if err := s.call(ctx, c.token, "getChat", map[string]any{"chat_id": chatParam(c.ChatRef)}, &chat); err != nil {
		res.Problem = "The bot can't see the channel yet. Add @" + me.Username + " to the channel as an admin."
		return res
	}
	res.ChannelTitle = chat.Title
	var member struct {
		Status          string `json:"status"`
		CanPostMessages bool   `json:"can_post_messages"`
	}
	if err := s.call(ctx, c.token, "getChatMember", map[string]any{"chat_id": chatParam(c.ChatRef), "user_id": me.ID}, &member); err == nil {
		res.IsAdmin = member.Status == "administrator" || member.Status == "creator"
		res.CanPost = member.Status == "creator" || (res.IsAdmin && member.CanPostMessages)
	}
	switch {
	case !res.IsAdmin:
		res.Problem = "Make @" + me.Username + " an admin of the channel."
	case !res.CanPost:
		res.Problem = "Give @" + me.Username + " the \"Post messages\" admin right."
	case !res.OwnerPaired:
		res.Problem = "Open @" + me.Username + " in Telegram and send /start " + c.PairCode + " so approvals reach you."
	}
	return res
}

// BotUsername looks up the bot's @username for a token.
func (s *Service) BotUsername(ctx context.Context, token string) (string, error) {
	var me tgUser
	if err := s.call(ctx, token, "getMe", nil, &me); err != nil {
		return "", err
	}
	return me.Username, nil
}

// ---------------------------------------------------------------------------
// Style learning
// ---------------------------------------------------------------------------

var (
	reMsgText = regexp.MustCompile(`(?s)<div class="tgme_widget_message_text js-message_text"[^>]*>(.*?)</div>`)
	reMsgID   = regexp.MustCompile(`data-post="[^"/]+/(\d+)"`)
	reBr      = regexp.MustCompile(`(?i)<br\s*/?>`)
	reTag     = regexp.MustCompile(`<[^>]+>`)
)

// FetchPublicPosts reads up to maxPosts recent posts (oldest first) from the
// channel's public preview page t.me/s/<name>.
func (s *Service) FetchPublicPosts(ctx context.Context, name string, maxPosts int) ([]string, error) {
	var all []string
	before := ""
	for page := 0; page < 4 && len(all) < maxPosts; page++ {
		u := "https://t.me/s/" + name
		if before != "" {
			u += "?before=" + before
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Sahand-CorText)")
		resp, err := s.client.Do(req)
		if err != nil {
			return all, err
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return all, fmt.Errorf("t.me returned HTTP %d", resp.StatusCode)
		}
		page := string(raw)
		var texts []string
		for _, m := range reMsgText.FindAllStringSubmatch(page, -1) {
			t := reBr.ReplaceAllString(m[1], "\n")
			t = strings.TrimSpace(html.UnescapeString(reTag.ReplaceAllString(t, "")))
			if t != "" {
				texts = append(texts, t)
			}
		}
		if len(texts) == 0 {
			break
		}
		all = append(texts, all...)
		ids := reMsgID.FindAllStringSubmatch(page, 1)
		if len(ids) == 0 || ids[0][1] == "1" {
			break
		}
		before = ids[0][1]
	}
	if len(all) > maxPosts {
		all = all[len(all)-maxPosts:]
	}
	return all, nil
}

// LearnStyle builds the channel's style guide from its own posts (public
// preview plus examples already saved) and any samples the owner pasted.
func (s *Service) LearnStyle(ctx context.Context, channelID string, pasted []string) (*Channel, int, error) {
	c, err := s.Store.Get(ctx, channelID)
	if err != nil {
		return nil, 0, err
	}
	posts := append([]string{}, c.Examples...)
	if name := publicName(c.ChatRef); name != "" {
		if fetched, err := s.FetchPublicPosts(ctx, name, 60); err == nil {
			posts = append(posts, fetched...)
		} else {
			log.Printf("[Channels] Reading t.me/s/%s failed: %v", name, err)
		}
	}
	for _, p := range pasted {
		if t := strings.TrimSpace(p); t != "" {
			posts = append(posts, t)
		}
	}
	posts = dedupeSubstantial(posts)
	if len(posts) == 0 {
		return c, 0, errors.New("no posts to learn from yet: publish a few, or paste some samples")
	}

	p, err := s.personaFor(ctx, c)
	if err != nil {
		return c, 0, err
	}
	prov, mod, err := s.resolve(p)
	if err != nil {
		return c, 0, err
	}
	var sb strings.Builder
	for i, t := range lastN(posts, 40) {
		fmt.Fprintf(&sb, "--- Post %d ---\n%s\n\n", i+1, clip(t, 1500))
	}
	sys := "You analyse a Telegram channel's posts and write a style guide another writer can follow to produce new posts indistinguishable from the owner's. Cover: language(s) and script, typical length, structure (hook, paragraphs, lists), tone and voice, emoji use, formatting habits, hashtags, links, calls to action and sign-offs, and recurring phrases. Be concrete, quote short typical phrases. Write the guide in English as short bullet points. No preamble."
	guide, _, err := s.router.Call(ctx, prov, s.keyFor(prov), mod, []provider.Message{
		{Role: "system", Content: sys},
		{Role: "user", Content: sb.String()},
	}, provider.CallOpts{})
	if err != nil {
		return c, 0, err
	}
	if err := s.Store.SetStyle(ctx, c.ID, strings.TrimSpace(guide), lastN(posts, maxExamples)); err != nil {
		return c, 0, err
	}
	c, err = s.Store.Get(ctx, c.ID)
	return c, len(posts), err
}

func dedupeSubstantial(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range in {
		t = strings.TrimSpace(t)
		if len([]rune(t)) < 40 || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

func lastN(in []string, n int) []string {
	if len(in) > n {
		return in[len(in)-n:]
	}
	return in
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// ---------------------------------------------------------------------------
// Writing posts
// ---------------------------------------------------------------------------

const maxPostRunes = 4000

func (s *Service) systemPrompt(ctx context.Context, c *Channel, p *persona.Persona) string {
	var sb strings.Builder
	if base := p.BuildCombinedSystemPrompt(); base != "" {
		sb.WriteString(base + "\n\n")
	}
	fmt.Fprintf(&sb, "=== YOUR JOB ===\nYou write posts for the owner's Telegram channel %q (%s). The owner gives you a subject and notes; you write one finished post that reads exactly as if the owner wrote it.\n", c.Name, c.ChatRef)
	sb.WriteString("Rules: reply with the post text only, no title line like \"Post:\", no quotes around it, no commentary. Plain text (Telegram shows no markdown), emojis allowed. Stay under 3500 characters. Write in the language the channel uses, unless the notes ask otherwise. Do not invent facts, numbers or links the notes don't give.\n")
	if ins := strings.TrimSpace(c.Instructions); ins != "" {
		sb.WriteString("\n=== OWNER'S STANDING INSTRUCTIONS FOR THIS CHANNEL ===\n" + ins + "\n")
	}
	if g := strings.TrimSpace(c.StyleGuide); g != "" {
		sb.WriteString("\n=== CHANNEL STYLE GUIDE ===\n" + g + "\n")
	}
	if len(c.Examples) > 0 {
		sb.WriteString("\n=== REAL POSTS FROM THIS CHANNEL (match their voice, length and format) ===\n")
		for i, ex := range lastN(c.Examples, 8) {
			fmt.Fprintf(&sb, "--- Example %d ---\n%s\n", i+1, clip(ex, 1200))
		}
	}
	if lessons, _ := s.Store.EditLessons(ctx, c.ID, 4); len(lessons) > 0 {
		sb.WriteString("\n=== HOW THE OWNER CORRECTED EARLIER DRAFTS (learn from these changes) ===\n")
		for i, l := range lessons {
			fmt.Fprintf(&sb, "--- Correction %d ---\nYour draft:\n%s\nWhat the owner published:\n%s\n", i+1, clip(l[0], 900), clip(l[1], 900))
		}
	}
	return sb.String()
}

// personaFor returns the channel's persona, or a blank one (main chat's
// active model, no extra instructions) when none is chosen.
func (s *Service) personaFor(ctx context.Context, c *Channel) (*persona.Persona, error) {
	if strings.TrimSpace(c.PersonaID) == "" {
		return &persona.Persona{}, nil
	}
	p, err := s.personas.Get(ctx, c.PersonaID)
	if err != nil {
		return nil, errors.New("the channel's persona no longer exists; choose another")
	}
	return p, nil
}

func (s *Service) write(ctx context.Context, c *Channel, post *Post, feedback string) (string, error) {
	p, err := s.personaFor(ctx, c)
	if err != nil {
		return "", err
	}
	prov, mod, err := s.resolve(p)
	if err != nil {
		return "", err
	}
	msgs := []provider.Message{{Role: "system", Content: s.systemPrompt(ctx, c, p)}}
	ask := "Subject: " + strings.TrimSpace(post.Idea)
	if n := strings.TrimSpace(post.Notes); n != "" {
		ask += "\n\nNotes:\n" + n
	}
	msgs = append(msgs, provider.Message{Role: "user", Content: ask})
	if fb := strings.TrimSpace(feedback); fb != "" && strings.TrimSpace(post.Draft) != "" {
		msgs = append(msgs,
			provider.Message{Role: "assistant", Content: post.Draft},
			provider.Message{Role: "user", Content: "Rewrite the post with these changes, and reply with the full new post only:\n" + fb})
	}
	out, _, err := s.router.Call(ctx, prov, s.keyFor(prov), mod, msgs, provider.CallOpts{})
	if err != nil {
		return "", err
	}
	out = cleanPost(out)
	if out == "" {
		return "", errors.New("the model returned an empty post")
	}
	return out, nil
}

func cleanPost(s string) string {
	s = strings.TrimSpace(s)
	for _, p := range []string{"Post:", "post:", "پست:"} {
		s = strings.TrimSpace(strings.TrimPrefix(s, p))
	}
	if len(s) > 1 && strings.HasPrefix(s, "\"") && strings.HasSuffix(s, "\"") {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	if r := []rune(s); len(r) > maxPostRunes {
		s = string(r[:maxPostRunes])
	}
	return s
}

func (s *Service) lockPost(id string) (func(), bool) {
	if _, busy := s.genLock.LoadOrStore(id, struct{}{}); busy {
		return nil, false
	}
	return func() { s.genLock.Delete(id) }, true
}

// Generate writes a new draft for an idea and stores it.
func (s *Service) Generate(ctx context.Context, channelID, idea, notes string) (*Post, error) {
	c, err := s.Store.Get(ctx, channelID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(idea) == "" {
		return nil, errors.New("write what the post is about")
	}
	post := &Post{ChannelID: c.ID, Idea: strings.TrimSpace(idea), Notes: strings.TrimSpace(notes)}
	text, err := s.write(ctx, c, post, "")
	if err != nil {
		return nil, err
	}
	post.Draft, post.AIDraft = text, text
	if err := s.Store.SavePost(ctx, post); err != nil {
		return nil, err
	}
	return post, nil
}

// Regenerate rewrites a draft, applying the owner's feedback when given.
func (s *Service) Regenerate(ctx context.Context, postID, feedback string) (*Post, error) {
	unlock, ok := s.lockPost(postID)
	if !ok {
		return nil, errors.New("this post is already being rewritten")
	}
	defer unlock()
	post, err := s.Store.GetPost(ctx, postID)
	if err != nil {
		return nil, err
	}
	if post.Status == StatusPosted || post.Status == StatusRemoved {
		return nil, errors.New("this post is already published")
	}
	c, err := s.Store.Get(ctx, post.ChannelID)
	if err != nil {
		return nil, err
	}
	text, err := s.write(ctx, c, post, feedback)
	if err != nil {
		return nil, err
	}
	post.Draft, post.AIDraft, post.Error = text, text, ""
	post.awaitingRev = false
	if post.Status == StatusDiscarded {
		post.Status = StatusDraft
	}
	if err := s.Store.SavePost(ctx, post); err != nil {
		return nil, err
	}
	// A post waiting in Telegram gets the new version there too.
	if post.Status == StatusPending {
		if err := s.sendForApproval(ctx, c, post); err != nil {
			return post, err
		}
	}
	return post, nil
}

// EditDraft saves the owner's hand edits.
func (s *Service) EditDraft(ctx context.Context, postID, text string) (*Post, error) {
	post, err := s.Store.GetPost(ctx, postID)
	if err != nil {
		return nil, err
	}
	if post.Status == StatusPosted || post.Status == StatusRemoved {
		return nil, errors.New("this post is already published")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("the post is empty")
	}
	post.Draft = text
	if err := s.Store.SavePost(ctx, post); err != nil {
		return nil, err
	}
	if post.Status == StatusPending {
		if c, err := s.Store.Get(ctx, post.ChannelID); err == nil {
			_ = s.sendForApproval(ctx, c, post)
		}
	}
	return post, nil
}

// ---------------------------------------------------------------------------
// Approval and publishing
// ---------------------------------------------------------------------------

func approvalKeyboard(postID string) map[string]any {
	return map[string]any{"inline_keyboard": [][]map[string]string{
		{{"text": "✅ Post", "callback_data": "cp:post:" + postID}, {"text": "✏️ Revise", "callback_data": "cp:rev:" + postID}},
		{{"text": "🗑 Discard", "callback_data": "cp:del:" + postID}},
	}}
}

// SendForApproval DMs the draft to the owner with Post / Revise / Discard.
func (s *Service) SendForApproval(ctx context.Context, postID string) (*Post, error) {
	post, err := s.Store.GetPost(ctx, postID)
	if err != nil {
		return nil, err
	}
	c, err := s.Store.Get(ctx, post.ChannelID)
	if err != nil {
		return nil, err
	}
	if post.Status == StatusPosted || post.Status == StatusRemoved {
		return nil, errors.New("this post is already published")
	}
	if err := s.sendForApproval(ctx, c, post); err != nil {
		return nil, err
	}
	return post, nil
}

func (s *Service) sendForApproval(ctx context.Context, c *Channel, post *Post) error {
	if c.OwnerID == 0 {
		return errors.New("pair your Telegram first: send /start " + c.PairCode + " to @" + c.BotUsername)
	}
	// Replace an earlier approval message so only the newest version has buttons.
	if post.approvalMsg != 0 {
		_ = s.call(ctx, c.token, "editMessageReplyMarkup", map[string]any{"chat_id": post.approvalChat, "message_id": post.approvalMsg, "reply_markup": map[string]any{"inline_keyboard": [][]any{}}}, nil)
	}
	head := "📝 Draft for " + c.Name + "\n\n"
	text := head + post.Draft
	if r := []rune(text); len(r) > 4096 {
		text = string(r[:4096])
	}
	var msg struct {
		MessageID int64 `json:"message_id"`
	}
	if err := s.call(ctx, c.token, "sendMessage", map[string]any{
		"chat_id": c.OwnerID, "text": text, "reply_markup": approvalKeyboard(post.ID),
		"link_preview_options": map[string]any{"is_disabled": true},
	}, &msg); err != nil {
		return err
	}
	post.approvalChat, post.approvalMsg = c.OwnerID, msg.MessageID
	post.Status, post.Error, post.awaitingRev = StatusPending, "", false
	return s.Store.SavePost(ctx, post)
}

// Publish posts the draft to the channel.
func (s *Service) Publish(ctx context.Context, postID string) (*Post, error) {
	unlock, ok := s.lockPost(postID)
	if !ok {
		return nil, errors.New("this post is busy, try again in a moment")
	}
	defer unlock()
	post, err := s.Store.GetPost(ctx, postID)
	if err != nil {
		return nil, err
	}
	if post.Status == StatusPosted {
		return post, nil
	}
	c, err := s.Store.Get(ctx, post.ChannelID)
	if err != nil {
		return nil, err
	}
	var msg struct {
		MessageID int64 `json:"message_id"`
	}
	if err := s.call(ctx, c.token, "sendMessage", map[string]any{"chat_id": chatParam(c.ChatRef), "text": post.Draft}, &msg); err != nil {
		post.Error = err.Error()
		_ = s.Store.SavePost(ctx, post)
		return post, err
	}
	post.Status, post.TGMessage, post.Error, post.awaitingRev = StatusPosted, msg.MessageID, "", false
	post.PostedAt = time.Now().UTC().Format("2006-01-02 15:04:05")
	if post.approvalMsg != 0 {
		_ = s.call(ctx, c.token, "editMessageReplyMarkup", map[string]any{"chat_id": post.approvalChat, "message_id": post.approvalMsg, "reply_markup": map[string]any{"inline_keyboard": [][]any{}}}, nil)
	}
	if err := s.Store.SavePost(ctx, post); err != nil {
		return post, err
	}
	_ = s.Store.AddExample(ctx, c.ID, post.Draft)
	return post, nil
}

// Discard throws a draft away (and clears its Telegram buttons).
func (s *Service) Discard(ctx context.Context, postID string) (*Post, error) {
	post, err := s.Store.GetPost(ctx, postID)
	if err != nil {
		return nil, err
	}
	if post.Status == StatusPosted {
		return nil, errors.New("this post is published; use Remove from channel")
	}
	if c, err := s.Store.Get(ctx, post.ChannelID); err == nil && post.approvalMsg != 0 {
		_ = s.call(ctx, c.token, "editMessageReplyMarkup", map[string]any{"chat_id": post.approvalChat, "message_id": post.approvalMsg, "reply_markup": map[string]any{"inline_keyboard": [][]any{}}}, nil)
	}
	post.Status, post.awaitingRev = StatusDiscarded, false
	return post, s.Store.SavePost(ctx, post)
}

// Unpublish deletes a published post from the channel.
func (s *Service) Unpublish(ctx context.Context, postID string) (*Post, error) {
	post, err := s.Store.GetPost(ctx, postID)
	if err != nil {
		return nil, err
	}
	if post.Status != StatusPosted {
		return nil, errors.New("this post isn't on the channel")
	}
	c, err := s.Store.Get(ctx, post.ChannelID)
	if err != nil {
		return nil, err
	}
	if err := s.call(ctx, c.token, "deleteMessage", map[string]any{"chat_id": chatParam(c.ChatRef), "message_id": post.TGMessage}, nil); err != nil {
		return post, err
	}
	post.Status = StatusRemoved
	return post, s.Store.SavePost(ctx, post)
}

// ---------------------------------------------------------------------------
// Bot polling: pairing, approval buttons, revisions, learning new posts
// ---------------------------------------------------------------------------

// Start keeps one poller per enabled channel bot running until ctx ends.
func (s *Service) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			s.sync(ctx)
			select {
			case <-ctx.Done():
				s.mu.Lock()
				for id, p := range s.pollers {
					p.cancel()
					delete(s.pollers, id)
				}
				s.mu.Unlock()
				return
			case <-t.C:
			case <-s.kick:
			}
		}
	}()
}

func (s *Service) sync(ctx context.Context) {
	chans, err := s.Store.List(ctx)
	if err != nil {
		return
	}
	want := map[string]string{}
	for _, c := range chans {
		if c.Enabled && c.token != "" {
			want[c.ID] = c.token
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, p := range s.pollers {
		if tok, ok := want[id]; !ok || tok != p.token {
			p.cancel()
			delete(s.pollers, id)
		}
	}
	for id, tok := range want {
		if _, ok := s.pollers[id]; !ok {
			pctx, cancel := context.WithCancel(ctx)
			s.pollers[id] = &poller{token: tok, cancel: cancel}
			go s.poll(pctx, id, tok)
		}
	}
}

type update struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		From tgUser `json:"from"`
		Chat struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"chat"`
		Text string `json:"text"`
	} `json:"message"`
	ChannelPost *struct {
		MessageID int64  `json:"message_id"`
		Text      string `json:"text"`
		Caption   string `json:"caption"`
	} `json:"channel_post"`
	Callback *struct {
		ID      string `json:"id"`
		From    tgUser `json:"from"`
		Data    string `json:"data"`
		Message *struct {
			MessageID int64 `json:"message_id"`
			Chat      struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"message"`
	} `json:"callback_query"`
}

func (s *Service) poll(ctx context.Context, channelID, token string) {
	log.Printf("[Channels] Bot started for channel %s", channelID)
	var offset int64
	for ctx.Err() == nil {
		var ups []update
		err := s.call(ctx, token, "getUpdates", map[string]any{
			"offset": offset, "timeout": 30,
			"allowed_updates": []string{"message", "callback_query", "channel_post"},
		}, &ups)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			var te *tgError
			if errors.As(err, &te) && strings.Contains(te.desc, "Conflict") {
				log.Printf("[Channels] Channel %s: another program is using this bot token; use a separate bot for the channel", channelID)
				sleepCtx(ctx, 60*time.Second)
			} else {
				sleepCtx(ctx, 5*time.Second)
			}
			continue
		}
		for _, u := range ups {
			offset = u.UpdateID + 1
			c, err := s.Store.Get(ctx, channelID)
			if err != nil {
				return
			}
			s.handle(ctx, c, u)
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func (s *Service) say(ctx context.Context, c *Channel, chatID int64, text string) {
	_ = s.call(ctx, c.token, "sendMessage", map[string]any{"chat_id": chatID, "text": text}, nil)
}

func (s *Service) handle(ctx context.Context, c *Channel, u update) {
	switch {
	case u.ChannelPost != nil:
		// The owner posted on the channel by hand: learn from it.
		text := u.ChannelPost.Text
		if text == "" {
			text = u.ChannelPost.Caption
		}
		if text != "" && !s.Store.IsOurMessage(ctx, c.ID, u.ChannelPost.MessageID) {
			_ = s.Store.AddExample(ctx, c.ID, text)
		}

	case u.Callback != nil:
		s.handleCallback(ctx, c, u)

	case u.Message != nil && u.Message.Chat.Type == "private":
		m := u.Message
		text := strings.TrimSpace(m.Text)
		if strings.HasPrefix(text, "/start") {
			code := strings.TrimSpace(strings.TrimPrefix(text, "/start"))
			if c.OwnerID == m.From.ID {
				s.say(ctx, c, m.Chat.ID, "✅ You're already paired with "+c.Name+". Drafts you send for approval arrive here.")
				return
			}
			if c.OwnerID == 0 && code != "" && strings.EqualFold(code, c.PairCode) {
				name := strings.TrimSpace(m.From.FirstName)
				if m.From.Username != "" {
					name += " (@" + m.From.Username + ")"
				}
				_ = s.Store.SetOwner(ctx, c.ID, m.From.ID, name)
				s.say(ctx, c, m.Chat.ID, "✅ Paired. Drafts for "+c.Name+" will come here with Post, Revise and Discard buttons.")
				return
			}
			s.say(ctx, c, m.Chat.ID, "⛔ This bot only works for the channel's owner.")
			return
		}
		if m.From.ID != c.OwnerID || c.OwnerID == 0 {
			s.say(ctx, c, m.Chat.ID, "⛔ This bot only works for the channel's owner.")
			return
		}
		post, _ := s.Store.AwaitingRevision(ctx, c.ID)
		if post == nil {
			s.say(ctx, c, m.Chat.ID, "Write new posts in the hub's Channels tab. When a draft arrives here, tap ✏️ Revise and send me what to change.")
			return
		}
		s.say(ctx, c, m.Chat.ID, "✍️ Rewriting…")
		go func(id, fb string) {
			rctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if _, err := s.Regenerate(rctx, id, fb); err != nil {
				s.say(rctx, c, m.Chat.ID, "⚠️ Couldn't rewrite it: "+err.Error())
			}
		}(post.ID, text)
	}
}

func (s *Service) handleCallback(ctx context.Context, c *Channel, u update) {
	cb := u.Callback
	answer := func(text string) {
		_ = s.call(ctx, c.token, "answerCallbackQuery", map[string]any{"callback_query_id": cb.ID, "text": text}, nil)
	}
	if c.OwnerID == 0 || cb.From.ID != c.OwnerID {
		answer("Only the channel owner can do that.")
		return
	}
	parts := strings.SplitN(cb.Data, ":", 3)
	if len(parts) != 3 || parts[0] != "cp" {
		answer("")
		return
	}
	post, err := s.Store.GetPost(ctx, parts[2])
	if err != nil || post.ChannelID != c.ID {
		answer("That draft no longer exists.")
		return
	}
	// Only the newest approval message for a post is live.
	if cb.Message != nil && post.approvalMsg != 0 && cb.Message.MessageID != post.approvalMsg {
		answer("There's a newer version of this draft below.")
		return
	}
	switch parts[1] {
	case "post":
		if post.Status == StatusPosted {
			answer("Already posted.")
			return
		}
		if _, err := s.Publish(ctx, post.ID); err != nil {
			answer("Couldn't post")
			s.say(ctx, c, c.OwnerID, "⚠️ Couldn't post to the channel: "+err.Error())
			return
		}
		answer("Posted ✅")
		s.say(ctx, c, c.OwnerID, "✅ Posted to "+c.Name+".")
	case "rev":
		if post.Status == StatusPosted {
			answer("Already posted.")
			return
		}
		// Only one draft waits for revision notes at a time.
		if prev, _ := s.Store.AwaitingRevision(ctx, c.ID); prev != nil && prev.ID != post.ID {
			prev.awaitingRev = false
			_ = s.Store.SavePost(ctx, prev)
		}
		post.awaitingRev = true
		_ = s.Store.SavePost(ctx, post)
		answer("")
		s.say(ctx, c, c.OwnerID, "✏️ What should I change? Send it as a message.")
	case "del":
		if _, err := s.Discard(ctx, post.ID); err != nil {
			answer(err.Error())
			return
		}
		answer("Discarded")
	default:
		answer("")
	}
}
