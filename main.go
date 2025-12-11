package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
)

// ---------- CONFIG ----------
var (
	botID           string
	db              *MemoryDB
	BucketPrefix    = "conv_"
	ContextMessages = 12

	cerebrasURL = "https://api.cerebras.ai/v1/chat/completions"
	cerebrasKey = os.Getenv("CEREBRAS_API_KEY")
	model       = "llama-3.3-70b"
)

// ---------- MEMORY ----------
type MemoryDB struct {
	Data map[string][]StoredMessage
}

type StoredMessage struct {
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}

func NewMemoryDB() *MemoryDB {
	return &MemoryDB{Data: make(map[string][]StoredMessage)}
}

func appendMemory(db *MemoryDB, key string, msg StoredMessage) error {
	db.Data[key] = append(db.Data[key], msg)
	return nil
}

func readLastMessages(db *MemoryDB, key string, count int) ([]StoredMessage, error) {
	msgs := db.Data[key]
	if len(msgs) <= count {
		return msgs, nil
	}
	return msgs[len(msgs)-count:], nil
}

// ---------- PROMPT BUILDER ----------
func buildPrompt(history []StoredMessage) []map[string]string {
	out := []map[string]string{}
	for _, h := range history {
		out = append(out, map[string]string{
			"role":    h.Role,
			"content": h.Content,
		})
	}
	return out
}

// ---------- LLM CALL ----------
func SendToLLM(url, key, model string, messages []map[string]string) (string, error) {
	body := map[string]interface{}{
		"model":    model,
		"messages": messages,
	}

	jsonBody, _ := json.Marshal(body)

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonBody))
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	client := &http.Client{Timeout: 25 * time.Second}

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var data map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&data)

	choices, ok := data["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return "", fmt.Errorf("LLM returned no choices")
	}

	msg := choices[0].(map[string]interface{})["message"].(map[string]interface{})
	return msg["content"].(string), nil
}

// ---------- UTILS ----------
func stripUserMention(text, botID string) string {
	return strings.ReplaceAll(text, "<@"+botID+">", "")
}

// ---------- MAIN ----------
func main() {
	token := os.Getenv("DISCORD_TOKEN")
	if token == "" {
		log.Fatal("DISCORD_TOKEN not set")
	}

	db = NewMemoryDB()

	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		log.Fatalf("Error creating Discord session: %v", err)
	}

	u, err := dg.User("@me")
	if err != nil {
		log.Fatalf("Error getting bot ID: %v", err)
	}
	botID = u.ID

	// ---------- MESSAGE HANDLER ----------
	dg.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		if m.Author == nil || m.Author.Bot {
			return
		}

		// detect @mention
		isMentioned := false
		for _, u := range m.Mentions {
			if u.ID == botID {
				isMentioned = true
				break
			}
		}

		// detect reply to bot
		isReplyToBot := false
		if m.MessageReference != nil && m.MessageReference.MessageID != "" {
			ref, err := s.ChannelMessage(m.ChannelID, m.MessageReference.MessageID)
			if err == nil && ref.Author != nil && ref.Author.ID == botID {
				isReplyToBot = true
			}
		}

		// reply overrides mention
		if isReplyToBot {
			isMentioned = false
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

	// ---------- START DISCORD ----------
	err = dg.Open()
	if err != nil {
		log.Fatalf("Error opening Discord connection: %v", err)
	}
	log.Println("Bot running...")

	// ---------- KEEP-ALIVE PORT FOR RENDER ----------
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	go func() {
		port := os.Getenv("PORT")
		if port == "" {
			port = "8080" // fallback for local testing
		}
		log.Println("Keep-alive server running on port " + port)
		log.Fatal(http.ListenAndServe(":"+port, nil))
	}()

	// ---------- SHUTDOWN ----------
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	_ = dg.Close()
}
