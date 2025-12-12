package gemini

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/yegors/co-atc/internal/ai"
	"github.com/yegors/co-atc/pkg/logger"
	"google.golang.org/genai"
)

// Client represents a Google Gemini API client.
type Client struct {
	apiKey string
	logger *logger.Logger
}

// NewClient creates a new Gemini Client.
func NewClient(apiKey string, logger *logger.Logger) *Client {
	return &Client{
		apiKey: apiKey,
		logger: logger.Named("gemini"),
	}
}

// -- RealtimeProvider Implementation --

func (c *Client) CreateRealtimeSession(ctx context.Context, config ai.RealtimeSessionConfig, systemPrompt string) (*ai.RealtimeSession, error) {
	return &ai.RealtimeSession{
		ID:           fmt.Sprintf("gemini_%d", time.Now().UnixNano()),
		ProviderID:   "gemini_live",
		CreatedAt:    time.Now().UTC(),
		ExpiresAt:    time.Now().Add(24 * time.Hour),
		Active:       true,
		LastActivity: time.Now().UTC(),
		SystemPrompt: systemPrompt,
		Config:       config,
	}, nil
}

// GeminiConnection wrapper that adapts OpenAI protocol to Gemini using genai SDK.
type GeminiConnection struct {
	session    *genai.Session
	readBuffer [][]byte
	mu         sync.Mutex

	currentText      string // For chat completion accumulation
	currentInputText string // ONLY for input transcription accumulation
	sampleRate       int
	logger           *logger.Logger

	// Mode switches
	transcriptionOnly bool

	// Input transcription finalization timer
	inputTxIdleTimer *time.Timer
	inputTxIdleMu    sync.Mutex

	// Concurrency control for Read()
	notifyCh chan struct{}
	_recvCh  chan recvMsg
}

type recvMsg struct {
	msg *genai.LiveServerMessage
	err error
}

func (c *GeminiConnection) Send(data []byte) error {
	// Parse OpenAI message.
	if len(data) == 0 {
		return nil
	}
	var oaMsg map[string]any
	if err := json.Unmarshal(data, &oaMsg); err != nil {
		c.logger.Error("Failed to parse OpenAI message", logger.Error(err))
		return nil
	}

	msgType, _ := oaMsg["type"].(string)

	switch msgType {
	case "input_audio_buffer.append":
		if audioBase64, ok := oaMsg["audio"].(string); ok {
			audioBytes, err := base64.StdEncoding.DecodeString(audioBase64)
			if err != nil {
				c.logger.Error("Failed to decode base64 audio", logger.Error(err))
				return nil
			}

			// Send to Gemini using SDK
			// Use generic "audio/pcm" MIME type as verified in standalone tests.
			err = c.session.SendRealtimeInput(genai.LiveRealtimeInput{
				Audio: &genai.Blob{
					Data:     audioBytes,
					MIMEType: "audio/pcm;rate=24000",
				},
			})
			if err != nil {
				return fmt.Errorf("gemini send audio failed: %w", err)
			}
		}

	case "session.update":
		// Handle system instruction updates by sending them as text messages
		sessionMap, ok := oaMsg["session"].(map[string]any)
		if !ok {
			return nil
		}
		instructions, ok := sessionMap["instructions"].(string)
		if !ok || instructions == "" {
			return nil
		}

		// Send as text message to context
		err := c.session.SendRealtimeInput(genai.LiveRealtimeInput{
			Text: "System Update: " + instructions,
		})
		if err != nil {
			c.logger.Error("Failed to send system update to Gemini", logger.Error(err))
			return fmt.Errorf("gemini send system update failed: %w", err)
		}
		return nil

	default:
		return nil
	}

	return nil
}

func (c *GeminiConnection) scheduleInputTxFinalize() {
	c.inputTxIdleMu.Lock()
	defer c.inputTxIdleMu.Unlock()

	// Reset timer each time we get a delta. When it fires, emit a "completed".
	if c.inputTxIdleTimer != nil {
		c.inputTxIdleTimer.Stop()
	}

	c.inputTxIdleTimer = time.AfterFunc(650*time.Millisecond, func() {
		c.mu.Lock()
		final := c.currentInputText
		c.currentInputText = ""
		c.mu.Unlock()

		final = strings.TrimSpace(final)
		if final == "" {
			return
		}

		oaMsg := map[string]any{
			"type":       "conversation.item.input_audio_transcription.completed",
			"transcript": final,
			"item_id":    "gemini-item-in",
		}
		b, err := json.Marshal(oaMsg)
		if err != nil {
			return
		}

		c.mu.Lock()
		c.readBuffer = append(c.readBuffer, b)
		notifyCh := c.notifyCh
		c.mu.Unlock()

		select {
		case notifyCh <- struct{}{}:
		default:
		}
	})
}

func (c *GeminiConnection) Read() (int, []byte, error) {
	// Start the receiver goroutine once.
	c.mu.Lock()
	if c.notifyCh == nil {
		c.notifyCh = make(chan struct{}, 1)
	}
	// Create receive channel only once.
	c.mu.Unlock()

	return c.readLoop()
}

// readLoop handles the continuous receiving of messages from the session.
func (c *GeminiConnection) readLoop() (int, []byte, error) {
	// Lazily start receiver goroutine exactly once.
	c.mu.Lock()
	if c._recvCh == nil {
		c._recvCh = make(chan recvMsg, 8)
		go func() {
			for {
				m, err := c.session.Receive()
				select {
				case c._recvCh <- recvMsg{msg: m, err: err}:
				default:
					// Drop if consumer is behind; keep connection alive.
				}
				if err != nil {
					return
				}
			}
		}()
	}
	recvCh := c._recvCh
	notifyCh := c.notifyCh
	c.mu.Unlock()

	for {
		// Serve from buffer first.
		c.mu.Lock()
		if len(c.readBuffer) > 0 {
			out := c.readBuffer[0]
			c.readBuffer = c.readBuffer[1:]
			c.mu.Unlock()
			return websocket.TextMessage, out, nil
		}
		c.mu.Unlock()

		select {
		case <-notifyCh:
			// Something was queued (e.g., timer completed). Loop will pop buffer.
			continue

		case r := <-recvCh:
			if r.err != nil {
				return 0, nil, r.err
			}
			msg := r.msg

			var newMessages [][]byte

			if msg != nil && msg.ServerContent != nil {
				msgJSON, _ := json.Marshal(msg)

				// 1) Input transcription deltas
				inputTx := jsonGetString(msgJSON, "serverContent", "inputTranscription", "text")
				if inputTx != "" {
					c.mu.Lock()
					c.currentInputText += inputTx
					c.mu.Unlock()

					c.scheduleInputTxFinalize()

					oaMsg := map[string]any{
						"type":          "conversation.item.input_audio_transcription.delta",
						"delta":         inputTx,
						"item_id":       "gemini-item-in",
						"content_index": 0,
					}
					if b, err := json.Marshal(oaMsg); err == nil {
						newMessages = append(newMessages, b)
					}
				}

				// 2) Output transcription deltas (chat mode only)
				if !c.transcriptionOnly {
					outputTx := jsonGetString(msgJSON, "serverContent", "outputTranscription", "text")
					if outputTx != "" {
						oaMsg := map[string]any{
							"type":          "response.audio_transcript.delta",
							"delta":         outputTx,
							"response_id":   "gemini-resp",
							"item_id":       "gemini-item-out-tx",
							"content_index": 0,
						}
						if b, err := json.Marshal(oaMsg); err == nil {
							newMessages = append(newMessages, b)
						}
					}
				}

				// 3) Model turn (chat mode only)
				if !c.transcriptionOnly && msg.ServerContent.ModelTurn != nil {
					for _, part := range msg.ServerContent.ModelTurn.Parts {
						if part == nil {
							continue
						}

						// Ignore "thought" parts to prevent chain-of-thought content from appearing in the UI.
						if part.Thought {
							continue
						}

						if part.Text != "" {
							c.mu.Lock()
							c.currentText += part.Text
							c.mu.Unlock()

							oaMsg := map[string]any{
								"type":          "response.text.delta",
								"delta":         part.Text,
								"response_id":   "gemini-resp",
								"item_id":       "gemini-item-text",
								"content_index": 0,
							}
							if b, err := json.Marshal(oaMsg); err == nil {
								newMessages = append(newMessages, b)
							}
						}

						if part.InlineData != nil && len(part.InlineData.Data) > 0 {
							audioB64 := base64.StdEncoding.EncodeToString(part.InlineData.Data)
							oaMsg := map[string]any{
								"type":          "response.audio.delta",
								"delta":         audioB64,
								"response_id":   "gemini-resp",
								"item_id":       "gemini-item-audio",
								"content_index": 0,
							}
							if b, err := json.Marshal(oaMsg); err == nil {
								newMessages = append(newMessages, b)
							}
						}
					}
				}

				// 4) Turn complete (chat mode only)
				turnComplete := jsonGetBool(msgJSON, "serverContent", "turnComplete")
				if turnComplete && !c.transcriptionOnly {
					c.mu.Lock()
					finalModelText := c.currentText
					c.currentText = ""
					c.mu.Unlock()

					oaMsg := map[string]any{
						"type": "response.text.done",
						"text": finalModelText,
					}
					if b, err := json.Marshal(oaMsg); err == nil {
						newMessages = append(newMessages, b)
					}

					oaTxDone := map[string]any{
						"type":        "response.audio_transcript.done",
						"response_id": "gemini-resp",
						"item_id":     "gemini-item-out-tx",
					}
					if b, err := json.Marshal(oaTxDone); err == nil {
						newMessages = append(newMessages, b)
					}

					oaAudioDone := map[string]any{
						"type":        "response.audio.done",
						"response_id": "gemini-resp",
						"item_id":     "gemini-item-audio",
					}
					if b, err := json.Marshal(oaAudioDone); err == nil {
						newMessages = append(newMessages, b)
					}
				}
			}

			if len(newMessages) > 0 {
				c.mu.Lock()
				c.readBuffer = append(c.readBuffer, newMessages...)
				out := c.readBuffer[0]
				c.readBuffer = c.readBuffer[1:]
				c.mu.Unlock()
				return websocket.TextMessage, out, nil
			}
		}
	}
}

func (c *GeminiConnection) Close() error {
	c.inputTxIdleMu.Lock()
	if c.inputTxIdleTimer != nil {
		c.inputTxIdleTimer.Stop()
	}
	c.inputTxIdleMu.Unlock()

	if c.session != nil {
		c.session.Close()
	}
	return nil
}

func (c *Client) ConnectSession(ctx context.Context, session *ai.RealtimeSession) (ai.AIConnection, error) {
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		Backend: genai.BackendGeminiAPI,
		APIKey:  c.apiKey,
		HTTPOptions: genai.HTTPOptions{
			APIVersion: "v1beta",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("genai.NewClient: %w", err)
	}

	modelName := session.Config.Model
	if modelName == "" {
		modelName = "gemini-2.5-flash-native-audio-preview-09-2025"
	}

	// Map Voice
	voiceName := session.Config.Voice
	if voiceName == "" {
		voiceName = "Puck" // Default voice
	}

	// Map Generation Config
	temp := float32(session.Config.Temperature)
	maxTokens := session.Config.MaxResponseTokens

	liveCfg := &genai.LiveConnectConfig{
		SystemInstruction: genai.NewContentFromText(session.SystemPrompt, "system"),
		ResponseModalities: []genai.Modality{
			genai.ModalityAudio,
		},
		SpeechConfig: &genai.SpeechConfig{
			VoiceConfig: &genai.VoiceConfig{
				PrebuiltVoiceConfig: &genai.PrebuiltVoiceConfig{
					VoiceName: voiceName,
				},
			},
		},
		Temperature:              &temp,
		MaxOutputTokens:          int32(maxTokens),
		InputAudioTranscription:  &genai.AudioTranscriptionConfig{},
		OutputAudioTranscription: &genai.AudioTranscriptionConfig{},
	}

	genaiSession, err := client.Live.Connect(ctx, modelName, liveCfg)
	if err != nil {
		return nil, fmt.Errorf("client.Live.Connect: %w", err)
	}

	c.logger.Info("Connected to Gemini Live", logger.String("model", modelName))

	return &GeminiConnection{
		session:           genaiSession,
		readBuffer:        make([][]byte, 0),
		sampleRate:        session.Config.SampleRate,
		logger:            c.logger,
		transcriptionOnly: false,
		notifyCh:          make(chan struct{}, 1),
	}, nil
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
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		Backend: genai.BackendGeminiAPI,
		APIKey:  c.apiKey,
		HTTPOptions: genai.HTTPOptions{
			APIVersion: "v1beta",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("genai.NewClient: %w", err)
	}

	modelName := session.Config.Model
	if modelName == "" {
		modelName = "gemini-2.5-flash-native-audio-preview-09-2025"
	}

	sysPrompt := session.Config.Prompt
	if strings.TrimSpace(sysPrompt) == "" {
		sysPrompt = "Transcribe the incoming audio accurately."
	}

	// Transcription only: We want NO ResponseModalities (no generation), just Input Transcription.
	liveCfg := &genai.LiveConnectConfig{
		SystemInstruction: genai.NewContentFromText(sysPrompt, "system"),
		// Do NOT request output audio/text modalities in a transcription service.
		// Leave nil so it is omitted.
		ResponseModalities:      []genai.Modality{genai.ModalityAudio},
		InputAudioTranscription: &genai.AudioTranscriptionConfig{},
		// Do not set OutputAudioTranscription
		// Do not set SpeechConfig
	}

	genaiSession, err := client.Live.Connect(ctx, modelName, liveCfg)
	if err != nil {
		return nil, fmt.Errorf("client.Live.Connect: %w", err)
	}

	c.logger.Info("Connected to Gemini Live (transcription)",
		logger.String("model", modelName))

	return &GeminiConnection{
		session:           genaiSession,
		readBuffer:        make([][]byte, 0),
		sampleRate:        session.Config.SampleRate,
		logger:            c.logger,
		transcriptionOnly: true,
		notifyCh:          make(chan struct{}, 1),
	}, nil
}

// -- ChatProvider Implementation --

func (c *Client) ChatCompletion(ctx context.Context, messages []ai.ChatMessage, config ai.ChatConfig) (string, error) {
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		Backend: genai.BackendGeminiAPI,
		APIKey:  c.apiKey,
		HTTPOptions: genai.HTTPOptions{
			APIVersion: "v1beta",
		},
	})
	if err != nil {
		return "", fmt.Errorf("genai.NewClient: %w", err)
	}

	model := config.Model
	if model == "" {
		model = "gemini-2.5-flash"
	}

	var fullSystemPrompt string
	var finalContents []*genai.Content
	var systemInstruction *genai.Content

	// First pass: extract and merge all system prompts
	for _, msg := range messages {
		if msg.Role == "system" {
			fullSystemPrompt += msg.Content + "\n"
		}
	}

	if fullSystemPrompt != "" {
		systemInstruction = genai.NewContentFromText(fullSystemPrompt, "system")
	}

	// Second pass: collect conversation history
	for _, msg := range messages {
		if msg.Role == "system" {
			continue
		}
		if msg.Role == "assistant" {
			finalContents = append(finalContents, genai.NewContentFromText(msg.Content, "model"))
		} else {
			finalContents = append(finalContents, genai.NewContentFromText(msg.Content, "user"))
		}
	}

	temp := float32(config.Temperature)
	genConfig := &genai.GenerateContentConfig{
		Temperature: &temp,
	}
	if config.MaxTokens > 0 {
		mt := int32(config.MaxTokens)
		genConfig.MaxOutputTokens = mt
	}
	if systemInstruction != nil {
		genConfig.SystemInstruction = systemInstruction
	}

	resp, err := client.Models.GenerateContent(ctx, model, finalContents, genConfig)
	if err != nil {
		return "", err
	}

	if len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
		return "", fmt.Errorf("no candidates in response")
	}

	var sb strings.Builder
	for _, p := range resp.Candidates[0].Content.Parts {
		if p == nil {
			continue
		}
		if p.Text != "" {
			sb.WriteString(p.Text)
		}
	}

	out := strings.TrimSpace(sb.String())
	if out == "" {
		return "", fmt.Errorf("no text content in response")
	}
	return out, nil
}

// Helper from user example
func jsonGetString(raw []byte, path ...string) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	cur := v
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur, ok = m[p]
		if !ok {
			return ""
		}
	}
	s, _ := cur.(string)
	return s
}

func jsonGetBool(raw []byte, path ...string) bool {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	cur := v
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		cur, ok = m[p]
		if !ok {
			return false
		}
	}
	b, _ := cur.(bool)
	return b
}
