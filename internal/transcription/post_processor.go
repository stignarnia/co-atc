package transcription

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yegors/co-atc/internal/ai"
	"github.com/yegors/co-atc/internal/storage/sqlite"
	"github.com/yegors/co-atc/internal/websocket"
	"github.com/yegors/co-atc/pkg/logger"
)

// PostProcessingConfig represents configuration for post-processing
type PostProcessingConfig struct {
	Enabled               bool
	Model                 string
	IntervalSeconds       int
	BatchSize             int
	ContextTranscriptions int
	SystemPromptPath      string
	TimeoutSeconds        int
}

// PostProcessingResult represents the structured result from the LLM
type PostProcessingResult struct {
	ProcessedContent string                      `json:"processed_content"`
	SpeakerType      string                      `json:"speaker_type,omitempty"`
	Callsign         string                      `json:"callsign,omitempty"`
	Clearances       []sqlite.ExtractedClearance `json:"clearances,omitempty"`
}

// TemplateRenderer is an interface for rendering templates with airspace data
type TemplateRenderer interface {
	RenderPostProcessorTemplate(templatePath string) (string, error)
}

// PostProcessor manages the post-processing of transcriptions
type PostProcessor struct {
	ctx                  context.Context
	cancel               context.CancelFunc
	transcriptionStorage *sqlite.TranscriptionStorage
	aircraftStorage      *sqlite.AircraftStorage
	clearanceStorage     *sqlite.ClearanceStorage
	chatProvider         ai.ChatProvider
	wsServer             *websocket.Server
	templateRenderer     TemplateRenderer
	logger               *logger.Logger
	config               PostProcessingConfig
	processingInterval   time.Duration
	batchSize            int
	wg                   sync.WaitGroup
	frequencyNames       map[string]string // Map of frequency IDs to names
}

// NewPostProcessor creates a new post-processor
func NewPostProcessor(
	ctx context.Context,
	transcriptionStorage *sqlite.TranscriptionStorage,
	aircraftStorage *sqlite.AircraftStorage,
	clearanceStorage *sqlite.ClearanceStorage,
	chatProvider ai.ChatProvider,
	wsServer *websocket.Server,
	templateRenderer TemplateRenderer,
	config PostProcessingConfig,
	logger *logger.Logger,
	frequencyNames map[string]string,
) (*PostProcessor, error) {
	if chatProvider == nil {
		return nil, fmt.Errorf("chat provider is required for post-processing")
	}

	// Create context with cancellation
	procCtx, procCancel := context.WithCancel(ctx)

	// Create post-processor
	processor := &PostProcessor{
		ctx:                  procCtx,
		cancel:               procCancel,
		transcriptionStorage: transcriptionStorage,
		aircraftStorage:      aircraftStorage,
		clearanceStorage:     clearanceStorage,
		chatProvider:         chatProvider,
		wsServer:             wsServer,
		templateRenderer:     templateRenderer,
		logger:               logger.Named("post-processor"),
		config:               config,
		processingInterval:   time.Duration(config.IntervalSeconds) * time.Second,
		batchSize:            config.BatchSize,
		frequencyNames:       frequencyNames,
	}

	return processor, nil
}

// Start starts the post-processing loop
func (p *PostProcessor) Start() error {
	if !p.config.Enabled {
		p.logger.Info("Post-processing is disabled, not starting")
		return nil
	}

	p.logger.Info("Starting post-processing loop",
		logger.Int("interval_seconds", p.config.IntervalSeconds),
		logger.Int("batch_size", p.batchSize))

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		ticker := time.NewTicker(p.processingInterval)
		defer ticker.Stop()

		for {
			select {
			case <-p.ctx.Done():
				p.logger.Info("Post-processing loop stopped due to context cancellation")
				return
			case <-ticker.C:
				if err := p.processNextBatch(); err != nil {
					p.logger.Error("Error processing batch", logger.Error(err))
				}
			}
		}
	}()
	return nil
}

// Stop stops the post-processing loop
func (p *PostProcessor) Stop() error {
	p.logger.Info("Stopping post-processing loop")
	p.cancel()
	p.wg.Wait()
	return nil
}

// TranscriptionBatch represents a batch of transcriptions to be processed
type TranscriptionBatch struct {
	ID               int64                       `json:"id"`
	Content          string                      `json:"content"`
	ContentProcessed string                      `json:"content_processed"`
	SpeakerType      string                      `json:"speaker_type"`
	Callsign         string                      `json:"callsign"`
	Clearances       []sqlite.ExtractedClearance `json:"clearances"`
	Timestamp        time.Time                   `json:"timestamp"`
}

// processNextBatch processes the next batch of unprocessed transcriptions
func (p *PostProcessor) processNextBatch() error {
	// Fetch unprocessed records from storage
	records, err := p.transcriptionStorage.GetUnprocessedTranscriptions(p.batchSize)
	if err != nil {
		return fmt.Errorf("failed to get unprocessed transcriptions: %w", err)
	}

	if len(records) == 0 {
		p.logger.Debug("No unprocessed transcriptions found")
		return nil // Nothing to process
	}

	p.logger.Debug("Processing batch of transcriptions", logger.Int("count", len(records)))

	// Get frequency name
	var frequencyName string
	var frequencyID string
	if len(records) > 0 {
		frequencyID = records[0].FrequencyID
		var err error
		frequencyName, err = p.getFrequencyName(frequencyID)
		if err != nil {
			p.logger.Error("Failed to get frequency name", logger.Error(err))
			frequencyName = frequencyID // Use ID as fallback
		}
	}

	// Get context
	var contextRecords []*sqlite.TranscriptionRecord
	if frequencyID != "" && p.config.ContextTranscriptions > 0 {
		contextRecords, err = p.transcriptionStorage.GetLastProcessedTranscriptions(frequencyID, p.config.ContextTranscriptions)
		if err != nil {
			p.logger.Error("Failed to get context transcriptions", logger.Error(err))
		} else {
			p.logger.Debug("Including context transcriptions", logger.Int("count", len(contextRecords)))
		}
	}

	// Prepare batch
	var batch []TranscriptionBatch
	for _, record := range contextRecords {
		batch = append(batch, TranscriptionBatch{
			ID:               record.ID,
			Content:          record.Content,
			ContentProcessed: record.ContentProcessed,
			SpeakerType:      record.SpeakerType,
			Callsign:         record.Callsign,
			Clearances:       []sqlite.ExtractedClearance{},
			Timestamp:        record.CreatedAt,
		})
	}

	for _, record := range records {
		batch = append(batch, TranscriptionBatch{
			ID:               record.ID,
			Content:          record.Content,
			ContentProcessed: "",
			SpeakerType:      "",
			Callsign:         "",
			Clearances:       []sqlite.ExtractedClearance{},
			Timestamp:        record.CreatedAt,
		})
	}
	p.sortBatchByTimestamp(batch)

	batchJSON, err := json.MarshalIndent(batch, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal transcription batch: %w", err)
	}

	systemPrompt, err := p.templateRenderer.RenderPostProcessorTemplate(p.config.SystemPromptPath)
	if err != nil {
		// handle failure
		p.markFailed(records, "[TEMPLATE_RENDER_FAILED]")
		return err
	}

	userInput := fmt.Sprintf("Radio Frequency:\n%s\n\nTransmissions Log:\n%s",
		frequencyName,
		string(batchJSON))

	results, err := p.processBatch(systemPrompt, userInput)
	if err != nil {
		p.markFailed(records, "[PROCESSING_FAILED]")
		return err
	}

	if len(results) == 0 {
		p.logger.Warn("No results returned from API")
		p.markFailed(records, "[NO_RESULTS_FROM_API]")
		return nil
	}

	// Update DB with processed results
	for _, result := range results {
		if result.ContentProcessed == "" {
			continue
		}

		isContextRecord := false
		for _, cr := range contextRecords {
			if cr.ID == result.ID {
				isContextRecord = true
				break
			}
		}
		if isContextRecord {
			continue
		}

		if err := p.transcriptionStorage.UpdateProcessedTranscription(
			result.ID, result.ContentProcessed, result.SpeakerType, result.Callsign,
		); err != nil {
			p.logger.Error("Failed to update", logger.Error(err))
			continue
		}

		if result.SpeakerType == "ATC" && len(result.Clearances) > 0 {
			for _, clearance := range result.Clearances {
				clearanceRecord := &sqlite.ClearanceRecord{
					TranscriptionID: result.ID,
					Callsign:        clearance.Callsign,
					ClearanceType:   clearance.Type,
					ClearanceText:   clearance.Text,
					Runway:          clearance.Runway,
					Timestamp:       result.Timestamp,
					Status:          "issued",
					CreatedAt:       time.Now().UTC(),
				}
				cid, err := p.clearanceStorage.StoreClearance(clearanceRecord)
				if err == nil {
					clearanceRecord.ID = cid
					p.broadcastClearanceEvent(clearanceRecord)
				}
			}
		}

		// find record and log
		var record *sqlite.TranscriptionRecord
		for _, r := range records {
			if r.ID == result.ID {
				record = r
				break
			}
		}
		if record != nil {
			record.ContentProcessed = result.ContentProcessed
			record.SpeakerType = result.SpeakerType
			record.Callsign = result.Callsign
			record.IsProcessed = true
			p.logProcessedTranscription(record)
		}
	}
	return nil
}

func (p *PostProcessor) markFailed(records []*sqlite.TranscriptionRecord, reason string) {
	for _, r := range records {
		p.transcriptionStorage.UpdateProcessedTranscription(r.ID, reason, "UNKNOWN", "")
	}
}

// processBatch processes a batch using ChatProvider
func (p *PostProcessor) processBatch(systemPrompt string, userInput string) ([]TranscriptionBatch, error) {
	messages := []ai.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userInput},
	}

	options := ai.ChatConfig{
		Model:       p.config.Model,
		Temperature: 0.0,
		MaxTokens:   4096,
	}

	content, err := p.chatProvider.ChatCompletion(p.ctx, messages, options)
	if err != nil {
		return nil, fmt.Errorf("chat completion failed: %w", err)
	}

	// Parse JSON from content
	startIdx := strings.Index(content, "[")
	endIdx := strings.LastIndex(content, "]")

	if startIdx == -1 || endIdx == -1 || startIdx >= endIdx {
		return nil, fmt.Errorf("response does not contain valid JSON array: %s", content)
	}

	jsonContent := content[startIdx : endIdx+1]

	var results []TranscriptionBatch
	if err := json.Unmarshal([]byte(jsonContent), &results); err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %w", err)
	}

	return results, nil
}

func (p *PostProcessor) getFrequencyName(frequencyID string) (string, error) {
	if name, ok := p.frequencyNames[frequencyID]; ok {

		return name, nil
	}
	return frequencyID, nil
}

func (p *PostProcessor) logProcessedTranscription(record *sqlite.TranscriptionRecord) {
	p.logger.Debug("Processed transcription", logger.Int64("id", record.ID))
	p.wsServer.Broadcast(&websocket.Message{
		Type: "transcription_update",
		Data: map[string]interface{}{
			"id":                record.ID,
			"frequency_id":      record.FrequencyID,
			"text":              record.Content,
			"timestamp":         record.CreatedAt,
			"is_complete":       true,
			"is_processed":      true,
			"content_processed": record.ContentProcessed,
			"speaker_type":      record.SpeakerType,
			"callsign":          record.Callsign,
		},
	})
}

func (p *PostProcessor) sortBatchByTimestamp(batch []TranscriptionBatch) {
	sort.Slice(batch, func(i, j int) bool {
		return batch[i].Timestamp.Before(batch[j].Timestamp)
	})
}

func (p *PostProcessor) broadcastClearanceEvent(clearance *sqlite.ClearanceRecord) {
	p.wsServer.Broadcast(&websocket.Message{
		Type: "clearance_issued",
		Data: map[string]interface{}{
			"id":             clearance.ID,
			"callsign":       clearance.Callsign,
			"clearance_type": clearance.ClearanceType,
			"clearance_text": clearance.ClearanceText,
			"runway":         clearance.Runway,
			"timestamp":      clearance.Timestamp,
			"status":         clearance.Status,
		},
	})
}
