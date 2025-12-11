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
	BucketPrefix     = "conv_"
	MaxMemoryEntries = 20
	ContextMessages  = 8
	RequestTimeout   = 30 * time.Second
)

const (
	ActivityTypePlaying   discordgo.ActivityType = 0
	ActivityTypeStreaming discordgo.ActivityType = 1
	ActivityTypeListening discordgo.ActivityType = 2
	ActivityTypeWatching  discordgo.ActivityType = 3
)

type StoredMessage struct {
	Role      string    `json:"role"`
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
	model := mustEnv("MODEL")

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
		log.Printf("Connected as %s", botID)
	})

	// ---- FIXED Handler (no more double replies) ----
	dg.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		// ignore bot messages ALWAYS
		if m.Author == nil || m.Author.Bot {
			return
		}

		// ----- Detect if user mentioned bot -----
		isMentioned := false
		for _, u := range m.Mentions {
			if u.ID == botID {
				isMentioned = true
				break
			}
		}

		// ----- Detect if user replied to bot -----
		isReplyToBot := false
		if m.MessageReference != nil && m.MessageReference.MessageID != "" {
			ref, err := s.ChannelMessage(m.ChannelID, m.MessageReference.MessageID)
			if err == nil && ref.Author != nil && ref.Author.ID == botID {
				isReplyToBot = true
			}
		}

		// ignore anything that isn't a direct ping or reply to the bot
		if !isMentioned && !isReplyToBot {
			return
		}

		// ---- HARD FIX: prevent double firing due to reply reference ghost events ----
		if m.MessageReference != nil && m.MessageReference.MessageID == m.ID {
			return
		}

		// ---- Memory key ----
		convKey := BucketPrefix + m.ChannelID

		userMsg := StoredMessage{
			Role:      "user",
			Content:   strings.TrimSpace(stripUserMention(m.Content, botID)),
			Timestamp: time.Now().UTC(),
		}
		_ = appendMemory(db, convKey, userMsg)

		history, _ := readLastMessages(db, convKey, ContextMessages)
		prompt := buildPrompt(history)

		reply, err := SendToLLM(cerebrasURL, cerebrasKey, model, prompt)
		if err != nil {
			log.Printf("LLM error: %v", err)
			return
		}
		if strings.TrimSpace(reply) == "" {
			return
		}

		assistantMsg := StoredMessage{
			Role:      "assistant",
			Content:   reply,
			Timestamp: time.Now().UTC(),
		}
		_ = appendMemory(db, convKey, assistantMsg)

		_, _ = s.ChannelMessageSendReply(m.ChannelID, reply, m.Reference())
	})

	// ---- Slash command: setstatus ----
	dg.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if i.Type != discordgo.InteractionApplicationCommand {
			return
		}
		if i.ApplicationCommandData().Name != "setstatus" {
			return
		}

		opts := i.ApplicationCommandData().Options
		if len(opts) < 3 {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "Usage: /setstatus <status> <type> <name>",
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
			actType = ActivityTypePlaying
		case "streaming":
			actType = ActivityTypeStreaming
		case "listening":
			actType = ActivityTypeListening
		case "watching":
			actType = ActivityTypeWatching
		default:
			actType = ActivityTypePlaying
		}

		err := s.UpdateStatusComplex(discordgo.UpdateStatusData{
			Status: status,
			Activities: []*discordgo.Activity{
				{Name: actName, Type: actType},
			},
		})

		msg := "Updated."
		if err != nil {
			msg = "Error: " + err.Error()
		}

		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Content: msg},
		})
	})

	if err := dg.Open(); err != nil {
		log.Fatalf("dg.Open: %v", err)
	}
	defer dg.Close()

	// Slash command registration
	_, _ = dg.ApplicationCommandCreate(dg.State.User.ID, "", &discordgo.ApplicationCommand{
		Name:        "setstatus",
		Description: "Change your bot status",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "status",
				Description: "online/idle/dnd",
				Required:    true,
			},
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "activitytype",
				Description: "playing/streaming/listening/watching",
				Required:    true,
			},
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "activityname",
				Description: "name",
				Required:    true,
			},
		},
	})

	// keepalive server
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	go func() {
		http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("alive"))
		})
		http.ListenAndServe(":"+port, nil)
	}()

	log.Println("Bot running.")
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("Shutting down.")
}

// --- helpers ---

func stripUserMention(content, botID string) string {
	forms := []string{
		fmt.Sprintf("<@%s>", botID),
		fmt.Sprintf("<@!%s>", botID),
	}
	out := content
	for _, m := range forms {
		out = strings.ReplaceAll(out, m, "")
	}
	return strings.TrimSpace(out)
}

func appendMemory(db *bolt.DB, convKey string, msg StoredMessage) error {
	return db.Update(func(tx *bolt.Tx) error {
		b, _ := tx.CreateBucketIfNotExists([]byte(convKey))
		key := []byte(fmt.Sprintf("%020d", time.Now().UTC().UnixNano()))
		val, _ := json.Marshal(msg)
		b.Put(key, val)

		// Trim
		c := b.Cursor()
		var keys [][]byte
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			keys = append(keys, append([]byte(nil), k...))
		}
		if len(keys) > MaxMemoryEntries {
			for i := 0; i < len(keys)-MaxMemoryEntries; i++ {
				b.Delete(keys[i])
			}
		}
		return nil
	})
}

func readLastMessages(db *bolt.DB, convKey string, n int) ([]StoredMessage, error) {
	var out []StoredMessage
	db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(convKey))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		var keys [][]byte
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			keys = append(keys, append([]byte(nil), k...))
		}

		start := 0
		if len(keys) > n {
			start = len(keys) - n
		}

		for _, k := range keys[start:] {
			v := b.Get(k)
			var sm StoredMessage
			_ = json.Unmarshal(v, &sm)
			out = append(out, sm)
		}

		return nil
	})

	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	return out, nil
}

func buildPrompt(history []StoredMessage) string {
	sys := "You are a discord member in a semi-toxic server named Vanir, you use gen alpha slang, keep replies short."
	out := []string{"SYSTEM: " + sys}
	for _, m := range history {
		out = append(out, m.Content)
	}
	return strings.Join(out, "\n\n")
}

func SendToLLM(url, apiKey, model, prompt string) (string, error) {
	payload := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": "You are a discord member in a semi-toxic server."},
			{"role": "user", "content": prompt},
		},
		"max_tokens": 512,
	}
	body, _ := json.Marshal(payload)

	ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("llm returned status %d: %s", resp.StatusCode, string(data))
	}

	var obj map[string]any
	if json.Unmarshal(data, &obj) == nil {
		if ch, ok := obj["choices"].([]any); ok && len(ch) > 0 {
			msg := ch[0].(map[string]any)["message"].(map[string]any)
			return strings.TrimSpace(msg["content"].(string)), nil
		}
	}

	s := strings.TrimSpace(string(data))
	if s == "" {
		return "", errors.New("empty response from LLM")
	}
	return s, nil
}
