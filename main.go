// main.go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	bolt "go.etcd.io/bbolt"
)

const (
	DBPath           = "memory.db"
	BucketPrefix     = "conv_"    // bucket per conversation (channel or user)
	MaxMemoryEntries = 20         // keep last N messages
	ContextMessages  = 8          // how many messages to send into LLM
	RequestTimeout   = 30 * time.Second
)

type StoredMessage struct {
	Role      string    `json:"role"` // "user" or "assistant" or "system"
	Content   string    `json:"content"`
	Timestamp time.Time `json:"ts"`
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("env %s required", key)
	}
	return v
}

func main() {
	discordToken := mustEnv("DISCORD_TOKEN")
	cerebrasURL := mustEnv("CEREBRAS_API_URL")
	cerebrasKey := mustEnv("CEREBRAS_API_KEY")
	model := os.Getenv("MODEL")
	if model == "" {
		model = "default"
	}
	botName := os.Getenv("BOT_NAME")
	if botName == "" {
		botName = "ai-bot"
	}

	db, err := bolt.Open(DBPath, 0600, nil)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	dg, err := discordgo.New("Bot " + discordToken)
	if err != nil {
		log.Fatalf("discordgo.New: %v", err)
	}

	var botID string

	dg.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		botID = s.State.User.ID
		log.Printf("Connected as: %s#%s (ID %s)", s.State.User.Username, s.State.User.Discriminator, botID)
	})

	// --- AI message handler ---
	dg.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		if m.Author == nil || m.Author.Bot {
			return
		}
		isMentioned := false
		for _, u := range m.Mentions {
			if u.ID == botID {
				isMentioned = true
				break
			}
		}
		isReplyToBot := false
		if m.MessageReference != nil && m.MessageReference.MessageID != "" {
			ref, err := s.ChannelMessage(m.ChannelID, m.MessageReference.MessageID)
			if err == nil && ref.Author != nil && ref.Author.ID == botID {
				isReplyToBot = true
			}
		}
		if !isMentioned && !isReplyToBot {
			return
		}

		convKey := BucketPrefix + m.ChannelID
		userMsg := StoredMessage{
			Role:      "user",
			Content:   strings.TrimSpace(stripUserMention(m.Content, botID)),
			Timestamp: time.Now().UTC(),
		}
		if err := appendMemory(db, convKey, userMsg); err != nil {
			log.Printf("appendMemory user: %v", err)
		}

		history, err := readLastMessages(db, convKey, ContextMessages)
		if err != nil {
			log.Printf("readLastMessages: %v", err)
		}

		prompt := buildPrompt(history, m.Author.Username, botName)

		replyText, err := SendToLLM(cerebrasURL, cerebrasKey, model, prompt)
		if err != nil {
			log.Printf("LLM error: %v", err)
			_, _ = s.ChannelMessageSendReply(m.ChannelID, "Sorry, LLM error: "+err.Error(), m.Reference())
			return
		}

		assistantMsg := StoredMessage{
			Role:      "assistant",
			Content:   replyText,
			Timestamp: time.Now().UTC(),
		}
		if err := appendMemory(db, convKey, assistantMsg); err != nil {
			log.Printf("appendMemory assistant: %v", err)
		}

		_, err = s.ChannelMessageSendReply(m.ChannelID, replyText, m.Reference())
		if err != nil {
			log.Printf("send reply failed: %v", err)
		}
	})

	// --- Slash command for status/activity ---
	dg.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if i.Type != discordgo.InteractionApplicationCommand {
			return
		}

		switch i.ApplicationCommandData().Name {
		case "setstatus":
			opts := i.ApplicationCommandData().Options
			if len(opts) < 3 {
				s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseChannelMessageWithSource,
					Data: &discordgo.InteractionResponseData{
						Content: "Usage: /setstatus <status> <activityType> <activityName>",
					},
				})
				return
			}

			status := opts[0].StringValue()
			actTypeStr := opts[1].StringValue()
			actName := opts[2].StringValue()

			var actType discordgo.ActivityType
			switch strings.ToLower(actTypeStr) {
			case "playing":
				actType = discordgo.Game
			case "streaming":
				actType = discordgo.Streaming
			case "listening":
				actType = discordgo.Listening
			case "watching":
				actType = discordgo.Watching
			default:
				actType = discordgo.Game
			}

			err := s.UpdateStatusComplex(discordgo.UpdateStatusData{
				Status: status, // online, idle, dnd
				Activities: []*discordgo.Activity{
					{
						Name: actName,
						Type: actType,
					},
				},
			})
			resp := "Status updated!"
			if err != nil {
				resp = "Failed to update status: " + err.Error()
			}
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: resp,
				},
			})
		}
	})

	if err := dg.Open(); err != nil {
		log.Fatalf("dg.Open: %v", err)
	}
	defer dg.Close()

	// --- Register slash command ---
	_, err = dg.ApplicationCommandCreate(dg.State.User.ID, "", &discordgo.ApplicationCommand{
		Name:        "setstatus",
		Description: "Change bot status/activity",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "status",
				Description: "online, idle, dnd",
				Required:    true,
			},
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "activitytype",
				Description: "playing, streaming, listening, watching",
				Required:    true,
			},
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "activityname",
				Description: "The activity name",
				Required:    true,
			},
		},
	})
	if err != nil {
		log.Println("Cannot create slash command:", err)
	}

	// --- Render keepalive webserver ---
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	go func() {
		http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("alive"))
		})
		log.Printf("Keepalive webserver listening on :%s", port)
		if err := http.ListenAndServe(":"+port, nil); err != nil {
			log.Fatalf("keepalive server error: %v", err)
		}
	}()

	log.Println("Bot is running. Press CTRL-C to exit.")
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("Shutting down.")
}

// --- helpers ---
func stripUserMention(content, botID string) string {
	if content == "" || botID == "" {
		return content
	}
	mentionForms := []string{
		fmt.Sprintf("<@%s>", botID),
		fmt.Sprintf("<@!%s>", botID),
	}
	out := content
	for _, m := range mentionForms {
		out = strings.ReplaceAll(out, m, "")
	}
	return strings.TrimSpace(out)
}

func appendMemory(db *bolt.DB, convKey string, msg StoredMessage) error {
	return db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(convKey))
		if err != nil {
			return err
		}
		key := []byte(fmt.Sprintf("%020d", time.Now().UTC().UnixNano()))
		val, _ := json.Marshal(msg)
		if err := b.Put(key, val); err != nil {
			return err
		}
		c := b.Cursor()
		var keys [][]byte
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			kCopy := make([]byte, len(k))
			copy(kCopy, k)
			keys = append(keys, kCopy)
		}
		if len(keys) <= MaxMemoryEntries {
			return nil
		}
		toRemove := len(keys) - MaxMemoryEntries
		for i := 0; i < toRemove; i++ {
			if err := b.Delete(keys[i]); err != nil {
				return err
			}
		}
		return nil
	})
}

func readLastMessages(db *bolt.DB, convKey string, n int) ([]StoredMessage, error) {
	var out []StoredMessage
	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(convKey))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		var keys [][]byte
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			kCopy := make([]byte, len(k))
			copy(kCopy, k)
			keys = append(keys, kCopy)
		}
		start := 0
		if len(keys) > n {
			start = len(keys) - n
		}
		for i := start; i < len(keys); i++ {
			v := b.Get(keys[i])
			if len(v) == 0 {
				continue
			}
			var sm StoredMessage
			if err := json.Unmarshal(v, &sm); err != nil {
				continue
			}
			out = append(out, sm)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	return out, nil
}

func buildPrompt(history []StoredMessage, username, botName string) string {
	sys := fmt.Sprintf("You are %s, an assistant in a Discord server. Be concise, helpful, and friendly. Keep replies suitable for chat.", botName)
	parts := []string{"SYSTEM: " + sys}
	for _, m := range history {
		role := strings.ToUpper(m.Role)
		t := m.Timestamp.Format(time.RFC3339)
		parts = append(parts, fmt.Sprintf("%s [%s]: %s", role, t, m.Content))
	}
	return strings.Join(parts, "\n\n")
}

func SendToLLM(url, apiKey, model, prompt string) (string, error) {
	payload := map[string]any{
		"model":      model,
		"input":      prompt,
		"stream":     false,
		"max_tokens": 512,
	}
	bodyBytes, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("llm returned status %d: %s", resp.StatusCode, string(respBytes))
	}

	var obj map[string]any
	if err := json.Unmarshal(respBytes, &obj); err == nil {
		for _, k := range []string{"output", "text", "result", "reply", "content"} {
			if v, ok := obj[k]; ok {
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s), nil
				}
			}
		}
	}

	s := strings.TrimSpace(string(respBytes))
	if s == "" {
		return "", errors.New("empty response from LLM")
	}
	return s, nil
}
