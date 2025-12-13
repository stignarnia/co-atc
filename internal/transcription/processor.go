package transcription

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/yegors/co-atc/internal/ai"
	"github.com/yegors/co-atc/internal/audio"
	"github.com/yegors/co-atc/internal/storage/sqlite"
	"github.com/yegors/co-atc/internal/websocket"
	"github.com/yegors/co-atc/pkg/logger"
)

// Import the logger package's exported functions
var (
	String = logger.String
	Int    = logger.Int
	Int64  = logger.Int64
	Error  = logger.Error
)

// Processor manages transcription for a specific frequency using a provided reader
type Processor struct {
	frequencyID         string
	audioReader         io.ReadCloser
	provider            ai.TranscriptionProvider
	wsServer            *websocket.Server
	storage             *sqlite.TranscriptionStorage
	ctx                 context.Context
	cancel              context.CancelFunc
	logger              *logger.Logger
	audioChunker        *audio.AudioChunker
	session             *ai.TranscriptionSession
	conn                ai.AIConnection
	chunkCount          int
	chunkCountMu        sync.Mutex
	transcriptionConfig Config
	sessionStartTime    time.Time
	sessionRefreshMu    sync.Mutex
	templateRenderer    TemplateRenderer
}

// NewProcessor creates a new transcription processor with a provided reader
func NewProcessor(
	ctx context.Context,
	frequencyID string,
	audioReader io.ReadCloser,
	config Config,
	wsServer *websocket.Server,
	storage *sqlite.TranscriptionStorage,
	logger *logger.Logger,
	provider ai.TranscriptionProvider,
	templateRenderer TemplateRenderer,
) (ProcessorInterface, error) {
	if provider == nil {
		return nil, fmt.Errorf("transcription provider is required")
	}

	procCtx, procCancel := context.WithCancel(ctx)

	// Create processor
	processor := &Processor{
		frequencyID:         frequencyID,
		audioReader:         audioReader,
		provider:            provider,
		wsServer:            wsServer,
		storage:             storage,
		ctx:                 procCtx,
		cancel:              procCancel,
		logger:              logger.Named("custom-xscribe").With(String("frequency_id", frequencyID)),
		audioChunker:        audio.NewAudioChunker(config.FFmpegSampleRate, config.FFmpegChannels, config.ChunkMs),
		transcriptionConfig: config,
		templateRenderer:    templateRenderer,
	}

	return processor, nil
}

// Start starts the transcription processor
func (p *Processor) Start() error {
	p.logger.Info("Starting custom transcription processor",
		String("frequency_id", p.frequencyID))

	// Render prompt template
	prompt, err := p.templateRenderer.RenderTranscriptionTemplate(p.transcriptionConfig.PromptPath)
	if err != nil {
		p.logger.Error("Failed to render transcription prompt template", Error(err))
		prompt = p.transcriptionConfig.Prompt // Fallback to raw string if template fails (though it might be empty context)
	}

	// Map Config to ai.TranscriptionConfig
	aiConfig := ai.TranscriptionConfig{
		Language:       p.transcriptionConfig.Language,
		Prompt:         prompt,
		Model:          p.transcriptionConfig.Model,
		NoiseReduction: p.transcriptionConfig.NoiseReduction,
		SampleRate:     p.transcriptionConfig.FFmpegSampleRate,
	}

	// Create transcription session
	p.session, err = p.provider.CreateTranscriptionSession(p.ctx, aiConfig)
	if err != nil {
		p.audioReader.Close()
		return fmt.Errorf("failed to create transcription session: %w", err)
	}
	p.logger.Info("Created transcription session", String("session_id", p.session.ID))

	// Record session start time
	p.sessionStartTime = time.Now()

	// Connect to Provider
	p.conn, err = p.provider.ConnectTranscriptionSession(p.ctx, p.session)
	if err != nil {
		p.audioReader.Close()
		return fmt.Errorf("failed to connect to Provider: %w", err)
	}
	p.logger.Info("Connected to Provider for transcription")

	// Start processing in goroutines
	go p.processAudio()
	go p.processTranscriptions()
	go p.monitorSessionDuration()

	return nil
}

// Stop stops the transcription processor
func (p *Processor) Stop() error {
	p.logger.Info("Stopping custom transcription processor")

	// Cancel context to stop all operations
	p.cancel()

	// Close WebSocket connection
	if p.conn != nil {
		p.conn.Close()
	}

	// Close audio reader
	if p.audioReader != nil {
		p.audioReader.Close()
	}

	return nil
}

// processAudio processes audio from the reader
func (p *Processor) processAudio() {
	p.logger.Info("Starting audio processing")

	// Create buffer for audio chunks
	buffer := make([]byte, 4096)

	// Track consecutive errors for backoff
	consecutiveErrors := 0
	maxConsecutiveErrors := 5
	reconnectAttempted := false

	for {
		select {
		case <-p.ctx.Done():
			p.logger.Info("Audio processing stopped due to context cancellation")
			return
		default:
			// Read from audio source
			n, err := p.audioReader.Read(buffer)
			if err != nil {
				if err == io.EOF {
					p.logger.Info("Audio source ended")
					return
				}
				p.logger.Error("Error reading from audio source", Error(err))
				return
			}

			if n > 0 {
				// Process audio chunk
				chunks, err := p.audioChunker.ProcessChunk(buffer[:n])
				if err != nil {
					p.logger.Error("Error processing audio chunk", Error(err))
					continue
				}

				// Send chunks to Provider
				for _, chunk := range chunks {
					// Base64 encode the chunk
					encoded := base64.StdEncoding.EncodeToString(chunk)

					// Send to Provider
					if err := p.sendAudioChunk(encoded); err != nil {
						consecutiveErrors++

						if consecutiveErrors <= 2 {
							p.logger.Error("Error sending audio chunk", Error(err), Int("consecutive_errors", consecutiveErrors))
						} else if consecutiveErrors == 3 {
							p.logger.Warn("Multiple consecutive errors sending audio chunks, will attempt reconnection soon", Error(err))
						}

						if consecutiveErrors >= maxConsecutiveErrors && !reconnectAttempted {
							p.logger.Info("Too many consecutive errors, attempting to reconnect")
							if err := p.reconnect(); err != nil {
								p.logger.Error("Failed to reconnect", Error(err))
								reconnectAttempted = true
							} else {
								p.logger.Info("Successfully reconnected")
								consecutiveErrors = 0
								reconnectAttempted = false
							}
						}

						if consecutiveErrors > 0 {
							backoffMs := 100 * (1 << uint(min(consecutiveErrors-1, 6)))
							time.Sleep(time.Duration(backoffMs) * time.Millisecond)
						}
						continue
					}

					// Reset error counter
					if consecutiveErrors > 0 {
						p.logger.Info("Audio chunk sending recovered", Int("previous_errors", consecutiveErrors))
						consecutiveErrors = 0
						reconnectAttempted = false
					}
				}
			}
		}
	}
}

// min helper (redefined here if not global)
func min(x, y int) int {
	if x < y {
		return x
	}
	return y
}

// sendAudioChunk sends an audio chunk to Provider
func (p *Processor) sendAudioChunk(encodedChunk string) error {
	// Use standard JSON format for audio chunks
	// Note: Gemini Client Adapter mimics "input_audio_buffer.append".
	// So we can stick to OpenAI JSON format here.
	message := map[string]any{
		"type":  "input_audio_buffer.append",
		"audio": encodedChunk,
	}

	data, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("failed to marshal audio chunk: %w", err)
	}

	p.chunkCountMu.Lock()
	p.chunkCount++
	chunkCount := p.chunkCount
	p.chunkCountMu.Unlock()

	if chunkCount%100 == 0 {
		p.logger.Debug("Sending audio chunk", Int("chunk_number", chunkCount))
	}

	if err := p.conn.Send(data); err != nil {
		return fmt.Errorf("failed to send: %w", err)
	}

	return nil
}

// processTranscriptions processes transcription events
func (p *Processor) processTranscriptions() {
	p.logger.Info("Starting transcription processing",
		String("frequency_id", p.frequencyID),
		String("session_id", p.session.ID))

	reconnectAttempts := 0
	maxReconnectAttempts := 5
	startBackoff := 1

	for {
		select {
		case <-p.ctx.Done():
			return
		default:
			_, message, err := p.conn.Read()
			if err != nil {
				// Handle error / reconnect logic
				// Simplified from original for brevity but keeping core logic
				if p.ctx.Err() != nil {
					return
				}

				p.logger.Warn("Connection issue", Error(err), Int("attempt", reconnectAttempts))

				if reconnectAttempts >= maxReconnectAttempts {
					p.logger.Error("Max reconnect attempts exceeded")
					time.Sleep(10 * time.Second)
					reconnectAttempts = 0
					continue
				}

				time.Sleep(time.Duration(startBackoff) * time.Second)
				if err := p.reconnect(); err != nil {
					p.logger.Error("Reconnect failed", Error(err))
					reconnectAttempts++
					startBackoff *= 2
				} else {
					reconnectAttempts = 0
					startBackoff = 1
				}
				continue
			}

			// Parse message
			var event map[string]any
			if err := json.Unmarshal(message, &event); err != nil {
				continue
			}

			// Process events (delta, completed, error)
			eventType, _ := event["type"].(string)
			switch eventType {
			case "conversation.item.input_audio_transcription.delta":
				// Handle partial transcript - log but don't send to WebSocket clients
				deltaText, _ := event["delta"].(string)
				if deltaText != "" {
					p.logger.Debug("Received delta transcription",
						String("frequency_id", p.frequencyID),
						String("text", deltaText))
				}

			case "conversation.item.input_audio_transcription.completed":
				// Handle completed transcript
				transcript, _ := event["transcript"].(string)
				if transcript == "" {
					p.logger.Error("Completed event missing transcript")
					continue
				}

				// Create transcription event
				transcriptionEvent := &TranscriptionEvent{
					Type:      "completed",
					Text:      transcript,
					Timestamp: time.Now().UTC(),
				}

				// Process the event
				if err := p.processTranscriptionEvent(transcriptionEvent); err != nil {
					p.logger.Error("Error processing completed transcription", Error(err))
				}

			case "error":
				p.logger.Error("Received error event from provider", String("raw_event", string(message)))
			}
		}
	}
}

// processTranscriptionEvent processes a transcription event
func (p *Processor) processTranscriptionEvent(event *TranscriptionEvent) error {
	// Log the event
	p.logger.Debug("Received completed transcription", String("text", event.Text))

	// Store completed transcriptions in the database
	if event.Type == "completed" {
		// Create record
		record := &sqlite.TranscriptionRecord{
			FrequencyID:      p.frequencyID,
			CreatedAt:        event.Timestamp,
			Content:          event.Text,
			IsComplete:       true,
			IsProcessed:      false,
			ContentProcessed: "",
		}

		// Store in database
		id, err := p.storage.StoreTranscription(record)
		if err != nil {
			return fmt.Errorf("failed to store transcription: %w", err)
		}

		p.logger.Debug("Stored transcription in database", Int64("id", id))

		// Send to WebSocket clients
		msg := &websocket.Message{
			Type: "transcription",
			Data: map[string]any{
				"id":                id,
				"frequency_id":      p.frequencyID,
				"text":              event.Text,
				"timestamp":         event.Timestamp,
				"is_complete":       true,
				"is_processed":      false,
				"content_processed": "",
			},
		}

		p.logger.Debug("Broadcasting transcription to WebSocket clients",
			String("frequency_id", p.frequencyID),
			String("text", event.Text),
			Int64("id", id))

		p.wsServer.Broadcast(msg)

		return nil
	}

	return nil
}

func (p *Processor) reconnect() error {
	p.sessionRefreshMu.Lock()
	defer p.sessionRefreshMu.Unlock()

	if p.conn != nil {
		p.conn.Close()
	}

	// Render prompt template
	prompt, err := p.templateRenderer.RenderTranscriptionTemplate(p.transcriptionConfig.PromptPath)
	if err != nil {
		p.logger.Error("Failed to render transcription prompt template on reconnect", Error(err))
		prompt = p.transcriptionConfig.Prompt
	}

	aiConfig := ai.TranscriptionConfig{
		Language:       p.transcriptionConfig.Language,
		Prompt:         prompt,
		Model:          p.transcriptionConfig.Model,
		NoiseReduction: p.transcriptionConfig.NoiseReduction,
	}

	p.session, err = p.provider.CreateTranscriptionSession(p.ctx, aiConfig)
	if err != nil {
		return err
	}

	p.sessionStartTime = time.Now()

	p.conn, err = p.provider.ConnectTranscriptionSession(p.ctx, p.session)
	return err
}

// monitorSessionDuration checks if the session duration limit is reached and refreshes if needed.
// OpenAI Realtime API has a 15-minute limit per session.
func (p *Processor) monitorSessionDuration() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	// Refresh 1 minute before the default 15 minute limit
	maxDuration := 14 * time.Minute

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.sessionRefreshMu.Lock()
			elapsed := time.Since(p.sessionStartTime)
			p.sessionRefreshMu.Unlock()

			if elapsed >= maxDuration {
				p.logger.Info("Max session duration approaching, forcing refresh", String("elapsed", elapsed.String()))
				if err := p.reconnect(); err != nil {
					p.logger.Error("Failed to refresh session", Error(err))
				}
			}
		}
	}
}
