package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/yegors/co-atc/internal/ai"
	"github.com/yegors/co-atc/pkg/logger"
)

// Client handles communication with OpenAI's APIs
type Client struct {
	apiKey     string
	model      string
	httpClient *http.Client
	logger     *logger.Logger
	baseURL    string // Stored without trailing slash

	// Separate paths if needed (optional)
	realtimeSessionPath      string
	realtimeWebsocketPath    string
	transcriptionSessionPath string
	chatCompletionsPath      string
}

// NewClient creates a new OpenAI client
func NewClient(apiKey string, logger *logger.Logger, baseURL string) *Client {
	// Determine base URL (prefer explicit parameter, then env, then default)
	base := strings.TrimRight(baseURL, "/")
	if base == "" {
		if env := os.Getenv("OPENAI_API_BASE"); env != "" {
			base = env
		} else {
			base = "https://api.openai.com"
		}
	}
	base = strings.TrimRight(base, "/")

	return &Client{
		apiKey:  apiKey,
		logger:  logger.Named("openai"),
		baseURL: base,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
		realtimeSessionPath:      "/v1/realtime/sessions",
		realtimeWebsocketPath:    "/v1/realtime",
		transcriptionSessionPath: "/v1/realtime/transcription_sessions",
		chatCompletionsPath:      "/v1/chat/completions",
	}
}

// SetPaths allows overriding specific endpoint paths
func (c *Client) SetPaths(realtimeSession, realtimeWs, transcriptionSession, chatCompletions string) {
	if realtimeSession != "" {
		c.realtimeSessionPath = realtimeSession
	}
	if realtimeWs != "" {
		c.realtimeWebsocketPath = realtimeWs
	}
	if transcriptionSession != "" {
		c.transcriptionSessionPath = transcriptionSession
	}
	if chatCompletions != "" {
		c.chatCompletionsPath = chatCompletions
	}
}

// -- RealtimeProvider Implementation --

// CreateRealtimeSession creates a new realtime session
func (c *Client) CreateRealtimeSession(ctx context.Context, config ai.RealtimeSessionConfig, systemPrompt string) (*ai.RealtimeSession, error) {
	if c.apiKey == "" {
		return nil, fmt.Errorf("OpenAI API key is required")
	}

	apiURL := c.baseURL + c.realtimeSessionPath

	// Map ai.RealtimeSessionConfig to OpenAI request
	reqBody := map[string]any{
		"model":               config.Model,
		"instructions":        systemPrompt,
		"modalities":          []string{"text", "audio"},
		"input_audio_format":  config.InputAudioFormat,
		"output_audio_format": config.OutputAudioFormat,
		"voice":               config.Voice,
	}

	if config.Temperature != 0 {
		reqBody["temperature"] = config.Temperature
	}

	if config.MaxResponseTokens > 0 {
		reqBody["max_response_output_tokens"] = config.MaxResponseTokens
	}

	if config.TurnDetection != "" && config.TurnDetection != "none" {
		reqBody["turn_detection"] = map[string]string{"type": config.TurnDetection}
	}

	jsonData, _ := json.Marshal(reqBody)

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("OpenAI-Beta", "realtime=v1")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to create session: %s %s", resp.Status, string(body))
	}

	var result struct {
		ID           string `json:"id"`
		ClientSecret struct {
			Value     string `json:"value"`
			ExpiresAt int64  `json:"expires_at"`
		} `json:"client_secret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return &ai.RealtimeSession{
		ID:           generateSessionID(),
		ProviderID:   result.ID,
		ClientSecret: result.ClientSecret.Value,
		CreatedAt:    time.Now().UTC(),
		ExpiresAt:    time.Unix(result.ClientSecret.ExpiresAt, 0),
		Active:       true,
		LastActivity: time.Now().UTC(),
		SystemPrompt: systemPrompt,
		Config:       config,
	}, nil
}

func generateSessionID() string {
	return fmt.Sprintf("atc_chat_%d", time.Now().UnixNano())
}

// OpenAIConnection wrapper for websocket
type OpenAIConnection struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (c *OpenAIConnection) Send(data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteMessage(websocket.TextMessage, data)
}

func (c *OpenAIConnection) Read() (int, []byte, error) {
	return c.conn.ReadMessage()
}

func (c *OpenAIConnection) Close() error {
	return c.conn.Close()
}

// ConnectSession establishes the WebSocket connection
func (c *Client) ConnectSession(ctx context.Context, session *ai.RealtimeSession) (ai.AIConnection, error) {
	wsBase := toWebSocketBase(c.baseURL)
	wsURL := fmt.Sprintf("%s%s", wsBase, c.realtimeWebsocketPath)

	dialer := websocket.Dialer{HandshakeTimeout: 30 * time.Second}
	headers := http.Header{}
	headers.Set("OpenAI-Beta", "realtime=v1")

	if session.ClientSecret != "" {
		headers.Set("Authorization", "Bearer "+session.ClientSecret)
	} else {
		headers.Set("Authorization", "Bearer "+c.apiKey)
	}

	conn, _, err := dialer.DialContext(ctx, wsURL, headers)
	if err != nil {
		return nil, err
	}

	return &OpenAIConnection{conn: conn}, nil
}

func (c *Client) ValidateSession(session *ai.RealtimeSession) bool {
	if session == nil {
		return false
	}
	if time.Now().UTC().After(session.ExpiresAt) {
		return false
	}
	return session.Active
}

// -- TranscriptionProvider Implementation --

func (c *Client) CreateTranscriptionSession(ctx context.Context, config ai.TranscriptionConfig) (*ai.TranscriptionSession, error) {
	apiURL := c.baseURL + c.transcriptionSessionPath

	// Create request body based on config
	type InputAudioTranscription struct {
		Model    string `json:"model"`
		Language string `json:"language,omitempty"`
		Prompt   string `json:"prompt,omitempty"`
	}

	reqBody := struct {
		InputAudioFormat         string                   `json:"input_audio_format"`
		InputAudioTranscription  *InputAudioTranscription `json:"input_audio_transcription"`
		InputAudioNoiseReduction any                      `json:"input_audio_noise_reduction,omitempty"`
		TurnDetection            any                      `json:"turn_detection,omitempty"`
	}{
		InputAudioFormat: "pcm16",
		InputAudioTranscription: &InputAudioTranscription{
			Model:    config.Model, // Use config model
			Language: config.Language,
			Prompt:   config.Prompt,
		},
	}

	if config.NoiseReduction != "" {
		reqBody.InputAudioNoiseReduction = map[string]string{"type": config.NoiseReduction}
	}

	// Default VAD setting if not specified logic could go here

	jsonData, _ := json.Marshal(reqBody)

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("OpenAI-Beta", "realtime=v1")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to create transcription session: %s %s", resp.Status, string(body))
	}

	var result struct {
		SessionID    string `json:"session_id"`
		ClientSecret struct {
			Value string `json:"value"`
		} `json:"client_secret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return &ai.TranscriptionSession{
		ID:           result.SessionID,
		ProviderID:   result.SessionID,
		ClientSecret: result.ClientSecret.Value,
		Config:       config,
	}, nil
}

func (c *Client) ConnectTranscriptionSession(ctx context.Context, session *ai.TranscriptionSession) (ai.AIConnection, error) {
	// Same as ConnectSession but strictly for transcription session parameters
	wsBase := toWebSocketBase(c.baseURL)
	// Transcription usually connects to /v1/realtime but with session_id/token
	// The path is configurable

	// The standard OpenAI Realtime path for transcription is usually same as chat (/v1/realtime)
	wsURL := fmt.Sprintf("%s%s", wsBase, c.realtimeWebsocketPath)

	dialer := websocket.Dialer{HandshakeTimeout: 30 * time.Second}
	headers := http.Header{}
	headers.Set("OpenAI-Beta", "realtime=v1")

	if session.ClientSecret != "" {
		headers.Set("Authorization", "Bearer "+session.ClientSecret)
	} else {
		headers.Set("Authorization", "Bearer "+c.apiKey)
	}

	// Assume standard client secret auth.

	conn, _, err := dialer.DialContext(ctx, wsURL, headers)
	if err != nil {
		return nil, err
	}

	return &OpenAIConnection{conn: conn}, nil
}

// -- ChatProvider Implementation --

func (c *Client) ChatCompletion(ctx context.Context, messages []ai.ChatMessage, config ai.ChatConfig) (string, error) {
	apiURL := c.baseURL + c.chatCompletionsPath

	type Message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}

	type Request struct {
		Model       string    `json:"model"`
		Messages    []Message `json:"messages"`
		MaxTokens   int       `json:"max_tokens,omitempty"`
		Temperature float64   `json:"temperature"`
	}

	reqMessages := make([]Message, len(messages))
	for i, msg := range messages {
		reqMessages[i] = Message{
			Role:    msg.Role,
			Content: msg.Content,
		}
	}

	reqBody := Request{
		Model:       config.Model,
		Messages:    reqMessages,
		MaxTokens:   config.MaxTokens,
		Temperature: config.Temperature,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("chat completion failed: %s %s", resp.Status, string(body))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	if len(result.Choices) == 0 {
		return "", fmt.Errorf("no choices in response")
	}

	return result.Choices[0].Message.Content, nil
}

// Helper
func toWebSocketBase(httpBase string) string {
	b := strings.TrimRight(httpBase, "/")
	if strings.HasPrefix(b, "https://") {
		return "wss://" + strings.TrimPrefix(b, "https://")
	} else if strings.HasPrefix(b, "http://") {
		return "ws://" + strings.TrimPrefix(b, "http://")
	}
	return "wss://" + b // Default to secure
}
