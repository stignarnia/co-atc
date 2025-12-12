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
	// DefaultHost is the default host for Gemini API.
	DefaultHost = "generativelanguage.googleapis.com"
	// DefaultPath is the WebSocket path for BidiGenerateContent.
	DefaultPath = "/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"
)

// Client represents a Google Gemini API client.
type Client struct {
	apiKey     string
	host       string
	logger     *logger.Logger
	dialer     *websocket.Dialer
	httpClient *http.Client
}

// NewClient creates a new Gemini Client.
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
	return &ai.RealtimeSession{
		ID:           fmt.Sprintf("gemini_%d", time.Now().UnixNano()),
		ProviderID:   "gemini_bidi",
		CreatedAt:    time.Now().UTC(),
		ExpiresAt:    time.Now().Add(24 * time.Hour),
		Active:       true,
		LastActivity: time.Now().UTC(),
		SystemPrompt: systemPrompt,
		Config:       config,
	}, nil
}

// GeminiConnection wrapper that adapts OpenAI protocol to Gemini.
type GeminiConnection struct {
	conn       *websocket.Conn
	mu         sync.Mutex
	readBuffer [][]byte
	logger     *logger.Logger

	currentText string // Buffer for accumulating text within a turn.
	sampleRate  int    // Sample rate for audio output/input.
}

func (c *GeminiConnection) Send(data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Parse OpenAI message.
	var oaMsg map[string]any
	if err := json.Unmarshal(data, &oaMsg); err != nil {
		c.logger.Error("Failed to parse OpenAI message", logger.Error(err))
		return nil // Ignore bad messages.
	}

	msgType, _ := oaMsg["type"].(string)

	switch msgType {
	case "input_audio_buffer.append":
		// OpenAI sends base64 encoded PCM16 audio.
		if audio, ok := oaMsg["audio"].(string); ok {
			geminiMsg := map[string]any{
				"realtimeInput": map[string]any{
					"audio": map[string]any{
						"mimeType": fmt.Sprintf("audio/pcm;rate=%d", c.sampleRate),
						"data":     audio,
					},
				},
			}
			return c.conn.WriteJSON(geminiMsg)
		}

	case "session.update":
		// Not mapped: Gemini Live does not support mid‑stream system instruction updates
		// via this adapter; ignored on purpose.
		return nil

	default:
		// Ignore unsupported message types.
		return nil
	}

	return nil
}

func (c *GeminiConnection) Read() (int, []byte, error) {
	// Serve from buffer first.
	c.mu.Lock()
	if len(c.readBuffer) > 0 {
		msg := c.readBuffer[0]
		c.readBuffer = c.readBuffer[1:]
		c.mu.Unlock()
		return websocket.TextMessage, msg, nil
	}
	c.mu.Unlock()

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

		// Handle setupComplete (optional: just ignore it for now).
		if _, ok := geminiMsg["setupComplete"]; ok {
			// No specific action required in this adapter.
		}

		// Handle serverContent: model output.
		if serverContent, ok := geminiMsg["serverContent"].(map[string]any); ok {
			// Transcription of input audio (if enabled on setup).
			if inputTx, ok := serverContent["inputTranscription"].(map[string]any); ok {
				if text, ok := inputTx["text"].(string); ok && text != "" {
					oaMsg := map[string]any{
						"type":          "conversation.item.input_audio_transcription.delta",
						"delta":         text,
						"item_id":       "gemini-item",
						"content_index": 0,
					}
					b, _ := json.Marshal(oaMsg)
					outputMessages = append(outputMessages, b)
				}
			}

			// Transcription of output audio (if enabled).
			if outputTx, ok := serverContent["outputTranscription"].(map[string]any); ok {
				if text, ok := outputTx["text"].(string); ok && text != "" {
					oaMsg := map[string]any{
						"type":          "conversation.item.input_audio_transcription.delta",
						"delta":         text,
						"item_id":       "gemini-item",
						"content_index": 0,
					}
					b, _ := json.Marshal(oaMsg)
					outputMessages = append(outputMessages, b)
				}
			}

			// Model turn content (text/audio parts).
			if modelTurn, ok := serverContent["modelTurn"].(map[string]any); ok {
				if parts, ok := modelTurn["parts"].([]any); ok {
					for _, p := range parts {
						part, ok := p.(map[string]any)
						if !ok {
							continue
						}

						// Text.
						if text, ok := part["text"].(string); ok && text != "" {
							c.mu.Lock()
							c.currentText += text
							c.mu.Unlock()

							oaMsg := map[string]any{
								"type":          "conversation.item.input_audio_transcription.delta",
								"delta":         text,
								"item_id":       "gemini-item",
								"content_index": 0,
							}
							b, _ := json.Marshal(oaMsg)
							outputMessages = append(outputMessages, b)
						}

						// Audio: inlineData with base64 PCM.
						if inlineData, ok := part["inlineData"].(map[string]any); ok {
							if data, ok := inlineData["data"].(string); ok && data != "" {
								oaMsg := map[string]any{
									"type":        "response.audio.delta",
									"delta":       data,
									"response_id": "gemini-resp",
									"item_id":     "gemini-item",
								}
								b, _ := json.Marshal(oaMsg)
								outputMessages = append(outputMessages, b)
							}
						}
					}
				}

				// Turn completion: flush accumulated text as response.text.done.
				if turnComplete, ok := serverContent["turnComplete"].(bool); ok && turnComplete {
					c.mu.Lock()
					finalText := c.currentText
					c.currentText = ""
					c.mu.Unlock()

					if finalText != "" {
						oaMsg := map[string]any{
							"type": "response.text.done",
							"text": finalText,
						}
						b, _ := json.Marshal(oaMsg)
						outputMessages = append(outputMessages, b)
					}
				}
			}
		}

		if len(outputMessages) > 0 {
			c.mu.Lock()
			if len(outputMessages) > 1 {
				c.readBuffer = append(c.readBuffer, outputMessages[1:]...)
			}
			c.mu.Unlock()
			return websocket.TextMessage, outputMessages[0], nil
		}
		// If no relevant output, keep reading.
	}
}

func (c *GeminiConnection) Close() error {
	return c.conn.Close()
}

func (c *Client) ConnectSession(ctx context.Context, session *ai.RealtimeSession) (ai.AIConnection, error) {
	u := url.URL{
		Scheme: "wss",
		Host:   c.host,
		Path:   DefaultPath,
	}

	q := u.Query()
	q.Set("key", c.apiKey)
	u.RawQuery = q.Encode()

	c.logger.Info("Connecting to Gemini Live API", logger.String("url", u.String()))

	conn, resp, err := c.dialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		if resp != nil {
			c.logger.Error(
				"Gemini WebSocket handshake failed",
				logger.Int("status_code", resp.StatusCode),
				logger.String("status", resp.Status),
			)
		}
		return nil, fmt.Errorf("failed to dial Gemini: %w", err)
	}

	voiceName := session.Config.Voice
	if voiceName == "" {
		voiceName = "Puck"
	}
	switch voiceName {
	case "alloy", "echo", "fable", "onyx", "nova", "shimmer":
		voiceName = "Puck"
	}

	modelName := session.Config.Model
	if modelName == "" {
		modelName = "gemini-1.5-flash"
	}
	if !stringsContains(modelName, "/") {
		modelName = "models/" + modelName
	}

	setupMsg := map[string]any{
		"setup": map[string]any{
			"model": modelName,
			"generationConfig": map[string]any{
				"responseModalities": []string{"AUDIO"},
				"speechConfig": map[string]any{
					"voiceConfig": map[string]any{
						"prebuiltVoiceConfig": map[string]any{
							"voiceName": voiceName,
						},
					},
				},
			},
			"systemInstruction": map[string]any{
				"parts": []map[string]any{
					{"text": session.SystemPrompt},
				},
			},
			// Optionally, enable input/output transcription here if you need it:
			// "inputAudioTranscription":  map[string]any{},
			// "outputAudioTranscription": map[string]any{},
		},
	}

	if err := conn.WriteJSON(setupMsg); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to send setup to Gemini: %w", err)
	}

	// NOTE: per spec you should wait for a message with "setupComplete"
	// before sending realtimeInput; that is handled by the caller using Read().

	return &GeminiConnection{
		conn:       conn,
		logger:     c.logger,
		readBuffer: make([][]byte, 0),
		sampleRate: session.Config.SampleRate,
	}, nil
}

func stringsContains(s, substr string) bool {
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
	// Live API configuration updates are not wired in this adapter.
	return nil
}

func (c *Client) EndSession(ctx context.Context, sessionID string) error {
	// Session ends when the WebSocket is closed by the caller.
	return nil
}

func (c *Client) ValidateSession(session *ai.RealtimeSession) bool {
	return session != nil && session.Active
}

// -- TranscriptionProvider Implementation --

func (c *Client) CreateTranscriptionSession(ctx context.Context, config ai.TranscriptionConfig) (*ai.TranscriptionSession, error) {
	return &ai.TranscriptionSession{
		ID:         fmt.Sprintf("gemini_transcription_%d", time.Now().UnixNano()),
		ProviderID: "gemini_bidi_transcription",
		Config:     config,
	}, nil
}

func (c *Client) ConnectTranscriptionSession(ctx context.Context, session *ai.TranscriptionSession) (ai.AIConnection, error) {
	u := url.URL{
		Scheme: "wss",
		Host:   c.host,
		Path:   DefaultPath,
	}
	q := u.Query()
	q.Set("key", c.apiKey)
	u.RawQuery = q.Encode()

	c.logger.Info("Connecting to Gemini Live API for Transcription", logger.String("url", u.String()))

	conn, resp, err := c.dialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		if resp != nil {
			c.logger.Error(
				"Gemini WebSocket handshake failed",
				logger.Int("status_code", resp.StatusCode),
				logger.String("status", resp.Status),
			)
		}
		return nil, fmt.Errorf("failed to dial Gemini: %w", err)
	}

	model := session.Config.Model
	if model == "" {
		model = "gemini-1.5-flash"
	}
	if !stringsContains(model, "/") {
		model = "models/" + model
	}

	systemText := "You are a transcriber. Transcribe the audio exactly. Do not add anything else."
	if session.Config.Prompt != "" {
		systemText = session.Config.Prompt
	}

	setupMsg := map[string]any{
		"setup": map[string]any{
			"model": model,
			"generationConfig": map[string]any{
				"responseModalities": []string{"TEXT"},
			},
			"systemInstruction": map[string]any{
				"parts": []map[string]any{
					{"text": systemText},
				},
			},
			"inputAudioTranscription": map[string]any{},
		},
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
	apiURL := fmt.Sprintf(
		"https://%s/v1beta/models/%s:generateContent?key=%s",
		c.host,
		config.Model,
		c.apiKey,
	)

	type Part struct {
		Text string `json:"text,omitempty"`
	}
	type Content struct {
		Role  string `json:"role,omitempty"`
		Parts []Part `json:"parts"`
	}

	var (
		geminiContents    []Content
		systemInstruction *Content
	)

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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewBuffer(jsonData))
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
