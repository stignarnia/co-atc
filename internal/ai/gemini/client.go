package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/yegors/co-atc/internal/ai"
	"github.com/yegors/co-atc/pkg/logger"
)

const (
	// DefaultHost is the default host for Gemini API
	DefaultHost = "generativelanguage.googleapis.com"
	// DefaultPath is the WebSocket path for BidiGenerateContent
	DefaultPath = "/ws/google.ai.generativelanguage.v1alpha.GenerativeService.BidiGenerateContent"
)

// Client represents a Google Gemini API client
type Client struct {
	apiKey     string
	host       string
	logger     *logger.Logger
	dialer     *websocket.Dialer
	httpClient *http.Client
}

// NewClient creates a new Gemini Client
func NewClient(apiKey string, logger *logger.Logger) *Client {
	return &Client{
		apiKey: apiKey,
		host:   DefaultHost,
		logger: logger.Named("gemini"),
		dialer: &websocket.Dialer{
			HandshakeTimeout: 30 * time.Second,
		},
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// -- RealtimeProvider Implementation --

func (c *Client) CreateRealtimeSession(ctx context.Context, config ai.RealtimeSessionConfig, systemPrompt string) (*ai.RealtimeSession, error) {
	// Gemini session is established via WebSocket directly.
	// We return a logical session object.
	return &ai.RealtimeSession{
		ID:           fmt.Sprintf("gemini_%d", time.Now().UnixNano()),
		ProviderID:   "gemini_bidi",
		CreatedAt:    time.Now().UTC(),
		ExpiresAt:    time.Now().Add(24 * time.Hour), // Dummy expiration
		Active:       true,
		LastActivity: time.Now().UTC(),
		SystemPrompt: systemPrompt,
		Config:       config,
	}, nil
}

// GeminiConnection wrapper that adapts OpenAI protocol to Gemini
type GeminiConnection struct {
	conn        *websocket.Conn
	mu          sync.Mutex
	readBuffer  [][]byte
	logger      *logger.Logger
	currentText string // Buffer for accumulating text within a turn
	sampleRate  int    // Sample rate for audio output
}

func (c *GeminiConnection) Send(data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Parse OpenAI message
	var oaMsg map[string]any
	if err := json.Unmarshal(data, &oaMsg); err != nil {
		c.logger.Error("Failed to parse OpenAI message", logger.Error(err))
		return nil // Ignore bad messages
	}

	msgType, _ := oaMsg["type"].(string)

	switch msgType {
	case "input_audio_buffer.append":
		// Extract audio and send to Gemini
		// OpenAI sends base64 encoded PCM16 (usually)
		if audio, ok := oaMsg["audio"].(string); ok {
			geminiMsg := map[string]any{
				"realtime_input": map[string]any{
					"media_chunks": []map[string]any{
						{
							"mime_type": fmt.Sprintf("audio/pcm;rate=%d", c.sampleRate),
							"data":      audio,
						},
					},
				},
			}
			return c.conn.WriteJSON(geminiMsg)
		}

	case "session.update":
		// Handled as best-effort for Gemini.
		// We can't easily update system instructions mid-stream for Bidi,
		// but we can log it.
		// c.logger.Debug("Received session.update (not fully supported)", logger.String("type", msgType))
		return nil

	default:
		// c.logger.Debug("Ignoring unsupported message type", logger.String("type", msgType))
		return nil
	}

	return nil
}

func (c *GeminiConnection) Read() (int, []byte, error) {
	c.mu.Lock()
	// Check buffer first
	if len(c.readBuffer) > 0 {
		msg := c.readBuffer[0]
		c.readBuffer = c.readBuffer[1:]
		c.mu.Unlock()
		return websocket.TextMessage, msg, nil
	}
	c.mu.Unlock()

	// Loop until we get a valid message to return
	for {
		_, msg, err := c.conn.ReadMessage()
		if err != nil {
			return 0, nil, err
		}

		var geminiMsg map[string]any
		if err := json.Unmarshal(msg, &geminiMsg); err != nil {
			continue
		}

		var outputMessages [][]byte

		// Check for server content (audio/text)
		if serverContent, ok := geminiMsg["serverContent"].(map[string]any); ok {
			if modelTurn, ok := serverContent["modelTurn"].(map[string]any); ok {
				if parts, ok := modelTurn["parts"].([]any); ok {
					for _, p := range parts {
						part := p.(map[string]any)

						// Text (Transcription)
						if text, ok := part["text"].(string); ok {
							if text != "" {
								c.mu.Lock()
								c.currentText += text
								c.mu.Unlock()

								// Send as conversation.item.input_audio_transcription.delta
								// This mimics the event processor.go expects for partial results
								oaMsg := map[string]any{
									"type":          "conversation.item.input_audio_transcription.delta",
									"delta":         text,
									"item_id":       "gemini-item",
									"content_index": 0,
								}
								bytes, _ := json.Marshal(oaMsg)
								outputMessages = append(outputMessages, bytes)
							}
						}

						// Audio
						if inlineData, ok := part["inlineData"].(map[string]any); ok {
							if data, ok := inlineData["data"].(string); ok {
								// Send as response.audio.delta
								oaMsg := map[string]any{
									"type":        "response.audio.delta",
									"delta":       data,
									"response_id": "gemini-resp",
									"item_id":     "gemini-item",
								}
								bytes, _ := json.Marshal(oaMsg)
								outputMessages = append(outputMessages, bytes)
							}
						}
					}

					// Check for turn completion to send "done" events if needed
					if turnComplete, ok := serverContent["turnComplete"].(bool); ok && turnComplete {
						c.mu.Lock()
						finalText := c.currentText
						c.currentText = "" // Reset
						c.mu.Unlock()

						if finalText != "" {
							// Send as response.text.done to trigger storage
							oaMsg := map[string]any{
								"type": "response.text.done",
								"text": finalText,
							}
							bytes, _ := json.Marshal(oaMsg)
							outputMessages = append(outputMessages, bytes)
						}
					}
				}
			}
		}

		if len(outputMessages) > 0 {
			c.mu.Lock()
			// If we generated multiple, queue rest
			if len(outputMessages) > 1 {
				c.readBuffer = append(c.readBuffer, outputMessages[1:]...)
			}
			c.mu.Unlock()
			return websocket.TextMessage, outputMessages[0], nil
		}
		// If no output, loop again
	}
}

func (c *GeminiConnection) Close() error {
	return c.conn.Close()
}

func (c *Client) ConnectSession(ctx context.Context, session *ai.RealtimeSession) (ai.AIConnection, error) {
	// Construct URL: wss://host/path?key=API_KEY
	u := url.URL{
		Scheme: "wss",
		Host:   c.host,
		Path:   DefaultPath,
	}

	q := u.Query()
	q.Set("key", c.apiKey)
	u.RawQuery = q.Encode()

	c.logger.Info("Connecting to Gemini Live API", logger.String("url", u.String()))

	// Dial
	conn, resp, err := c.dialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		if resp != nil {
			c.logger.Error("Gemini WebSocket handshake failed",
				logger.Int("status_code", resp.StatusCode),
				logger.String("status", resp.Status))
		}
		return nil, fmt.Errorf("failed to dial Gemini: %w", err)
	}

	// Send Setup Message immediately
	// Voice mapping: Allow 'Puck' or others.
	// Assume config.Voice is a Gemini voice or default.
	voiceName := session.Config.Voice
	if voiceName == "" {
		voiceName = "Puck"
	}
	// Map OpenAI voices to Gemini closest equivalents if strictly OpenAI names used
	switch voiceName {
	case "alloy", "echo", "fable", "onyx", "nova", "shimmer":
		voiceName = "Puck" // Fallback
	}

	setupMsg := map[string]any{
		"setup": map[string]any{
			"model": "models/" + session.Config.Model,
			"generation_config": map[string]any{
				"response_modalities": []string{"AUDIO"},
				"speech_config": map[string]any{
					"voice_config": map[string]any{
						"prebuilt_voice_config": map[string]any{
							"voice_name": voiceName,
						},
					},
				},
			},
			"system_instruction": map[string]any{
				"parts": []map[string]any{
					{"text": session.SystemPrompt},
				},
			},
		},
	}
	// Handle model prefix if missing
	if modelName, ok := setupMsg["setup"].(map[string]any)["model"].(string); ok {
		if !contains(modelName, "/") {
			setupMsg["setup"].(map[string]any)["model"] = "models/" + modelName
		}
	}

	if err := conn.WriteJSON(setupMsg); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to send setup to Gemini: %w", err)
	}

	return &GeminiConnection{
		conn:       conn,
		logger:     c.logger,
		readBuffer: make([][]byte, 0),
		sampleRate: session.Config.SampleRate,
	}, nil
}

func contains(s, substr string) bool {
	for i := 0; i < len(s); i++ {
		if hasPrefix(s[i:], substr) {
			return true
		}
	}
	return false
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[0:len(prefix)] == prefix
}

func (c *Client) UpdateSessionInstructions(ctx context.Context, sessionID string, instructions string) error {
	return nil
}

func (c *Client) EndSession(ctx context.Context, sessionID string) error {
	return nil
}

func (c *Client) ValidateSession(session *ai.RealtimeSession) bool {
	return session != nil && session.Active
}

// -- TranscriptionProvider Implementation --

func (c *Client) CreateTranscriptionSession(ctx context.Context, config ai.TranscriptionConfig) (*ai.TranscriptionSession, error) {
	// Gemini REST transcription is stateless / inline or uses regular websocket session
	// We return a logical session.
	return &ai.TranscriptionSession{
		ID:         fmt.Sprintf("gemini_transcription_%d", time.Now().UnixNano()),
		ProviderID: "gemini_bidi_transcription",
		Config:     config,
	}, nil
}

func (c *Client) ConnectTranscriptionSession(ctx context.Context, session *ai.TranscriptionSession) (ai.AIConnection, error) {
	// For streaming transcription, Gemini uses the same BidiStreaming endpoint.
	// We just need to ensure the setup message is correct for transcription (audio in -> text out).
	// The standard adapter logic handles "audio input" -> "delta text".
	// The Adapter currently handles "modelTurn" -> "audio delta".
	// We need to modify the Adapter / GeminiConnection to also handle "text delta" or return it.

	// Re-use Connect logic
	u := url.URL{
		Scheme: "wss",
		Host:   c.host,
		Path:   DefaultPath,
	}
	q := u.Query()
	q.Set("key", c.apiKey)
	u.RawQuery = q.Encode()

	c.logger.Info("Connecting to Gemini Live API for Transcription", logger.String("url", u.String()))

	conn, _, err := c.dialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to dial Gemini: %w", err)
	}

	// Send Setup Message for Transcription
	model := session.Config.Model // e.g. "gemini-1.5-flash-8b"
	if !contains(model, "/") {
		model = "models/" + model
	}

	setupMsg := map[string]interface{}{
		"setup": map[string]interface{}{
			"model": model,
			"generation_config": map[string]interface{}{
				"response_modalities": []string{"TEXT"},
			},
			"system_instruction": map[string]interface{}{
				"parts": []map[string]interface{}{
					{"text": "You are a transcriber. Transcribe the audio exactly. Do not add anything else."},
				},
			},
		},
	}
	if session.Config.Prompt != "" {
		setupMsg["setup"].(map[string]interface{})["system_instruction"] = map[string]interface{}{
			"parts": []map[string]interface{}{
				{"text": session.Config.Prompt},
			},
		}
	}

	if err := conn.WriteJSON(setupMsg); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to send setup to Gemini: %w", err)
	}

	return &GeminiConnection{
		conn:       conn,
		logger:     c.logger,
		readBuffer: make([][]byte, 0),
		sampleRate: session.Config.SampleRate,
	}, nil
}

// -- ChatProvider Implementation --

func (c *Client) ChatCompletion(ctx context.Context, messages []ai.ChatMessage, config ai.ChatConfig) (string, error) {
	apiURL := fmt.Sprintf("https://%s/v1beta/models/%s:generateContent?key=%s", c.host, config.Model, c.apiKey)

	type Part struct {
		Text string `json:"text,omitempty"`
	}
	type Content struct {
		Role  string `json:"role,omitempty"`
		Parts []Part `json:"parts"`
	}

	geminiContents := []Content{}
	var systemInstruction *Content

	for _, msg := range messages {
		if msg.Role == "system" {
			systemInstruction = &Content{
				Parts: []Part{{Text: msg.Content}},
			}
			continue
		}

		role := "user"
		if msg.Role == "assistant" {
			role = "model"
		}

		geminiContents = append(geminiContents, Content{
			Role:  role,
			Parts: []Part{{Text: msg.Content}},
		})
	}

	reqBody := map[string]any{
		"contents": geminiContents,
		"generationConfig": map[string]any{
			"temperature":     config.Temperature,
			"maxOutputTokens": config.MaxTokens,
		},
	}

	if systemInstruction != nil {
		reqBody["systemInstruction"] = systemInstruction
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

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("gemini chat failed: %s %s", resp.Status, string(body))
	}

	var result struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	if len(result.Candidates) > 0 && len(result.Candidates[0].Content.Parts) > 0 {
		return result.Candidates[0].Content.Parts[0].Text, nil
	}

	return "", fmt.Errorf("no content in gemini response")
}
