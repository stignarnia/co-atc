package adsb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/yegors/co-atc/pkg/logger"
)

// Client is responsible for fetching ADS-B data from the source
type Client struct {
	httpClient        *http.Client
	sourceType        string
	localSourceURL    string
	externalSourceURL string
	apiHost           string
	apiKey            string
	stationLat        float64
	stationLon        float64
	searchRadiusNM    float64
	logger            *logger.Logger

	// OpenSky specific options (preferred when source_type == "external-opensky")
	openskyCredsPath string
	openskyBBoxLamin float64
	openskyBBoxLomin float64
	openskyBBoxLamax float64
	openskyBBoxLomax float64

	// Cached OpenSky OAuth2 token (to reduce repeated token requests)
	token       string
	tokenExpiry time.Time
	tokenMu     sync.Mutex
}

// NewClient creates a new ADS-B client
// Note: accepts OpenSky-specific parameters (creds path + optional explicit bbox)
func NewClient(
	sourceType string,
	localSourceURL string,
	externalSourceURL string,
	apiHost string,
	apiKey string,
	stationLat float64,
	stationLon float64,
	searchRadiusNM float64,
	openskyCredsPath string,
	openskyBBoxLamin float64,
	openskyBBoxLomin float64,
	openskyBBoxLamax float64,
	openskyBBoxLomax float64,
	timeout time.Duration,
	loggerObj *logger.Logger,
) *Client {
	return &Client{
		sourceType:        sourceType,
		localSourceURL:    localSourceURL,
		externalSourceURL: externalSourceURL,
		apiHost:           apiHost,
		apiKey:            apiKey,
		stationLat:        stationLat,
		stationLon:        stationLon,
		searchRadiusNM:    searchRadiusNM,
		httpClient: &http.Client{
			Timeout: timeout,
		},
		logger:           loggerObj.Named("adsb-cli"),
		openskyCredsPath: openskyCredsPath,
		openskyBBoxLamin: openskyBBoxLamin,
		openskyBBoxLomin: openskyBBoxLomin,
		openskyBBoxLamax: openskyBBoxLamax,
		openskyBBoxLomax: openskyBBoxLomax,
	}
}

// FetchData fetches ADS-B data from the configured source
func (c *Client) FetchData(ctx context.Context) (*RawAircraftData, error) {
	switch c.sourceType {
	case "local":
		return c.fetchLocalData(ctx)
	case "external-adsbexchangelike":
		return c.fetchExternalData(ctx)
	case "external-opensky":
		return c.fetchOpenSkyData(ctx)
	default:
		return nil, fmt.Errorf("unknown source type: %s", c.sourceType)
	}
}

// fetchLocalData fetches data from the local source
func (c *Client) fetchLocalData(ctx context.Context) (*RawAircraftData, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.localSourceURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	c.logger.Debug("Fetching local ADS-B data",
		logger.String("url", c.localSourceURL),
	)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	var data RawAircraftData
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %w", err)
	}

	for i := range data.Aircraft {
		data.Aircraft[i].SourceType = "local"
	}

	c.logger.Debug("Successfully fetched local ADS-B data",
		logger.Int("aircraft_count", len(data.Aircraft)),
		logger.Int("message_count", data.Messages),
	)

	return &data, nil
}

// fetchExternalData fetches data from the external API (ADS-B Exchange / RapidAPI style)
func (c *Client) fetchExternalData(ctx context.Context) (*RawAircraftData, error) {
	urlStr := fmt.Sprintf(c.externalSourceURL, c.stationLat, c.stationLon, c.searchRadiusNM)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		c.logger.Error("Failed to create request", logger.Error(err), logger.String("url", urlStr))
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-rapidapi-host", c.apiHost)
	req.Header.Set("x-rapidapi-key", c.apiKey)

	c.logger.Debug("Fetching external ADS-B data",
		logger.String("url", urlStr),
		logger.String("host", c.apiHost),
	)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.logger.Error("Failed to execute request", logger.Error(err), logger.String("url", urlStr))
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.logger.Error("Unexpected status code",
			logger.Int("status_code", resp.StatusCode),
			logger.String("url", urlStr))
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.logger.Error("Failed to read response body", logger.Error(err))
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	bodyPreview := string(body)
	if len(bodyPreview) > 200 {
		bodyPreview = bodyPreview[:200] + "..."
	}
	c.logger.Debug("Response body preview", logger.String("body", bodyPreview))

	// Try parsing as external API format first (ExternalAPIResponse)
	var externalData ExternalAPIResponse
	if err := json.Unmarshal(body, &externalData); err != nil {
		c.logger.Debug("Failed to parse as external API format, trying standard format", logger.Error(err))

		var data RawAircraftData
		if err2 := json.Unmarshal(body, &data); err2 != nil {
			c.logger.Error("Failed to parse as both external and standard formats", logger.Error(err2))
			return nil, fmt.Errorf("failed to parse JSON: %w (external format) and %w (standard format)", err, err2)
		}

		c.logger.Debug("Parsed as standard format",
			logger.Int("aircraft_count", len(data.Aircraft)))
		return &data, nil
	}

	c.logger.Debug("Parsed as external API format",
		logger.Int("aircraft_count", len(externalData.AC)))

	aircraft := make([]ADSBTarget, 0, len(externalData.AC))
	for i, extTarget := range externalData.AC {
		if i == 0 {
			c.logger.Debug("Sample aircraft conversion",
				logger.String("hex", extTarget.Hex),
				logger.String("flight", extTarget.Flight),
				logger.Float64("alt_baro_converted", extTarget.AltBaro.Float64()),
				logger.Float64("lat_converted", extTarget.Lat.Float64()),
			)
		}
		aircraft = append(aircraft, extTarget.Convert())
	}

	data := &RawAircraftData{
		Now:      float64(time.Now().Unix()),
		Messages: externalData.Messages,
		Aircraft: aircraft,
	}

	if data.Aircraft == nil {
		data.Aircraft = []ADSBTarget{}
		c.logger.Warn("External API returned nil aircraft array, initializing empty array")
	}

	c.logger.Debug("Successfully fetched external ADS-B data",
		logger.Int("aircraft_count", len(data.Aircraft)),
		logger.String("source", "external API"),
	)

	return data, nil
}

// fetchOpenSkyData fetches ADS-B state vectors from the OpenSky REST API.
//
// OpenSky expects a bounding box defined by lamin, lomin, lamax, lomax.
// We prefer an explicitly-configured bbox; otherwise derive one from station coords + radius.
//
// Authentication:
// - If a credentials file contains an access_token field, use it directly.
// - Otherwise, if credentials contain client_id and client_secret, perform client_credentials token request.
// - If no credentials file is present, we perform an anonymous request (rate-limited / limited data).
func (c *Client) fetchOpenSkyData(ctx context.Context) (*RawAircraftData, error) {
	// Determine bounding box: prefer explicit OpenSky bbox, else derive from station + radius
	var lamin, lomin, lamax, lomax float64
	if c.openskyBBoxLamin != 0 || c.openskyBBoxLamax != 0 || c.openskyBBoxLomin != 0 || c.openskyBBoxLomax != 0 {
		lamin = c.openskyBBoxLamin
		lomin = c.openskyBBoxLomin
		lamax = c.openskyBBoxLamax
		lomax = c.openskyBBoxLomax
	} else {
		if c.searchRadiusNM <= 0 {
			return nil, fmt.Errorf("search radius must be positive for OpenSky bounding box derivation")
		}
		lat := c.stationLat
		lon := c.stationLon
		rad := c.searchRadiusNM

		latDeg := rad / 60.0
		lonDeg := rad / (60.0 * math.Cos(lat*math.Pi/180.0))

		lamin = lat - latDeg
		lamax = lat + latDeg
		lomin = lon - lonDeg
		lomax = lon + lonDeg
	}

	// Credentials path (prefer configured)
	credPath := c.openskyCredsPath
	if credPath == "" {
		credPath = "opensky/credentials.json"
	}

	// Local token variable used for request Authorization header (may be set from cache or newly obtained)
	var token string

	// First, check if we already have a non-expired cached token to reuse.
	c.tokenMu.Lock()
	if c.token != "" && time.Now().Before(c.tokenExpiry) {
		// Use cached token
		token = c.token
		c.tokenMu.Unlock()
	} else {
		// No valid cached token - try to obtain one from credentials file (or token endpoint).
		c.tokenMu.Unlock()

		var tokenCandidates string

		if _, err := os.Stat(credPath); err == nil {
			b, err := os.ReadFile(credPath)
			if err != nil {
				c.logger.Error("Failed to read OpenSky credentials file", logger.Error(err), logger.String("path", credPath))
				return nil, fmt.Errorf("failed to read opensky credentials: %w", err)
			}

			var credMap map[string]interface{}
			if err := json.Unmarshal(b, &credMap); err != nil {
				c.logger.Error("Failed to parse OpenSky credentials JSON", logger.Error(err))
				return nil, fmt.Errorf("invalid opensky credentials JSON: %w", err)
			}

			// Helper to pick first present non-empty string value from multiple possible keys.
			getFirstString := func(m map[string]interface{}, keys ...string) string {
				for _, k := range keys {
					if v, ok := m[k]; ok {
						switch vv := v.(type) {
						case string:
							if vv != "" {
								return vv
							}
						case []byte:
							if s := string(vv); s != "" {
								return s
							}
						}
					}
				}
				return ""
			}

			// Accept multiple possible key names (snake_case, kebab-case, camelCase).
			// Prefer any explicit access token first; otherwise fall back to client credentials.
			tokenCandidates = getFirstString(credMap, "access_token", "access-token", "accessToken")

			if tokenCandidates == "" {
				clientID := getFirstString(credMap, "client_id", "client-id", "clientId")
				clientSecret := getFirstString(credMap, "client_secret", "client-secret", "clientSecret")
				tokenURL := getFirstString(credMap, "token_url", "token-url", "tokenUrl")
				if tokenURL == "" {
					tokenURL = "https://auth.opensky-network.org/auth/realms/opensky-network/protocol/openid-connect/token"
				}

				if clientID != "" && clientSecret != "" {
					form := url.Values{}
					form.Set("grant_type", "client_credentials")
					form.Set("client_id", clientID)
					form.Set("client_secret", clientSecret)

					c.logger.Debug("Requesting OpenSky OAuth2 token", logger.String("token_url", tokenURL))
					resp, err := http.PostForm(tokenURL, form)
					if err != nil {
						c.logger.Error("Failed to request OpenSky token", logger.Error(err))
						return nil, fmt.Errorf("failed to request opensky token: %w", err)
					}
					defer resp.Body.Close()
					if resp.StatusCode != http.StatusOK {
						body, _ := io.ReadAll(resp.Body)
						c.logger.Error("OpenSky token endpoint returned non-200", logger.Int("status", resp.StatusCode), logger.String("body", string(body)))
						return nil, fmt.Errorf("opensky token endpoint error: %d", resp.StatusCode)
					}
					var tokResp struct {
						AccessToken string `json:"access_token"`
						ExpiresIn   int    `json:"expires_in"`
					}
					if err := json.NewDecoder(resp.Body).Decode(&tokResp); err != nil {
						c.logger.Error("Failed to decode OpenSky token response", logger.Error(err))
						return nil, fmt.Errorf("failed to decode opensky token response: %w", err)
					}
					if tokResp.AccessToken == "" {
						return nil, fmt.Errorf("opensky token response did not contain access_token")
					}
					// Use token and compute expiry
					tokenCandidates = tokResp.AccessToken

					// Determine expiry
					var expiry time.Time
					if tokResp.ExpiresIn > 60 {
						// Subtract a small safety margin
						expiry = time.Now().Add(time.Duration(tokResp.ExpiresIn-30) * time.Second)
					} else {
						expiry = time.Now().Add(29 * time.Minute)
					}

					// Cache token and expiry
					c.tokenMu.Lock()
					c.token = tokenCandidates
					c.tokenExpiry = expiry
					c.tokenMu.Unlock()
				} else {
					c.logger.Error("OpenSky credentials missing required fields (access_token or client_id/client_secret)")
					return nil, fmt.Errorf("opensky credentials must contain access_token or client_id+client_secret")
				}
			} else {
				// We found an explicit access token in the credentials file.
				// Cache it for a reasonable period (tokens from file may still expire; use conservative default).
				c.tokenMu.Lock()
				c.token = tokenCandidates
				c.tokenExpiry = time.Now().Add(29 * time.Minute)
				c.tokenMu.Unlock()
			}
		} else {
			// Credentials file absent - warn and proceed anonymously (rate limits and restrictions apply)
			c.logger.Warn("OpenSky credentials file not found - proceeding as anonymous (rate limits may apply)", logger.String("path", credPath))
		}

		// After attempting to load/cache, attempt to use cached token (if set)
		c.tokenMu.Lock()
		if c.token != "" && time.Now().Before(c.tokenExpiry) {
			token = c.token
		}
		c.tokenMu.Unlock()
	}

	// Build the request
	urlStr := fmt.Sprintf("https://opensky-network.org/api/states/all?lamin=%f&lomin=%f&lamax=%f&lomax=%f", lamin, lomin, lamax, lomax)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create OpenSky request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	c.logger.Debug("Fetching OpenSky ADS-B data", logger.String("url", urlStr))
	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.logger.Error("Failed to execute OpenSky request", logger.Error(err), logger.String("url", urlStr))
		return nil, fmt.Errorf("failed to execute opensky request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		c.logger.Error("Unexpected OpenSky status code", logger.Int("status_code", resp.StatusCode), logger.String("body", string(body)))
		return nil, fmt.Errorf("unexpected opensky status code: %d", resp.StatusCode)
	}

	var osResp struct {
		Time   int64           `json:"time"`
		States [][]interface{} `json:"states"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&osResp); err != nil {
		c.logger.Error("Failed to decode OpenSky response", logger.Error(err))
		return nil, fmt.Errorf("failed to parse opensky JSON: %w", err)
	}

	// Convert OpenSky states -> ADSBTarget
	aircraft := make([]ADSBTarget, 0, len(osResp.States))
	for i, s := range osResp.States {
		// Defensive extraction according to OpenSky docs
		var hex, callsign, squawk string
		var lat, lon, baroAltMeters, velocity, trueTrack, verticalRate float64
		var onGround bool
		var geoAltMeters float64
		var category float64

		if len(s) > 0 {
			if v, ok := s[0].(string); ok {
				hex = v
			}
		}
		if len(s) > 1 {
			if v, ok := s[1].(string); ok {
				callsign = v
			}
		}
		if len(s) > 5 {
			if v, ok := s[5].(float64); ok {
				lon = v
			}
		}
		if len(s) > 6 {
			if v, ok := s[6].(float64); ok {
				lat = v
			}
		}
		if len(s) > 7 {
			if v, ok := s[7].(float64); ok {
				baroAltMeters = v
			}
		}
		if len(s) > 8 {
			if v, ok := s[8].(bool); ok {
				onGround = v
			}
		}
		if len(s) > 9 {
			if v, ok := s[9].(float64); ok {
				velocity = v
			}
		}
		if len(s) > 10 {
			if v, ok := s[10].(float64); ok {
				trueTrack = v
			}
		}
		if len(s) > 11 {
			if v, ok := s[11].(float64); ok {
				verticalRate = v
			}
		}
		if len(s) > 13 {
			if v, ok := s[13].(float64); ok {
				geoAltMeters = v
			}
		}
		if len(s) > 14 {
			if v, ok := s[14].(string); ok {
				squawk = v
			}
		}
		if len(s) > 17 {
			if v, ok := s[17].(float64); ok {
				category = v
			}
		}

		// Convert units: meters -> feet, m/s -> knots, m/s -> ft/min
		altBaroFeet := baroAltMeters * 3.28084
		altGeomFeet := geoAltMeters * 3.28084
		gsKnots := velocity * 1.943844
		vertFPM := verticalRate * 196.850394

		target := ADSBTarget{
			Hex:        hex,
			Flight:     callsign,
			Type:       "", // OpenSky doesn't provide a direct aircraft type in states/all
			AltBaro:    altBaroFeet,
			AltGeom:    altGeomFeet,
			GS:         gsKnots,
			TAS:        gsKnots, // approximate: OpenSky only gives velocity (m/s)
			Track:      trueTrack,
			BaroRate:   vertFPM,
			Lat:        lat,
			Lon:        lon,
			Squawk:     squawk,
			Category:   fmt.Sprintf("%d", int(category)),
			SourceType: "external-opensky",
			OnGround:   &onGround,
		}

		// Log first couple conversions for debugging
		if i < 2 {
			c.logger.Debug("OpenSky aircraft conversion sample",
				logger.String("hex", target.Hex),
				logger.String("flight", target.Flight),
				logger.Float64("lat", target.Lat),
				logger.Float64("lon", target.Lon),
			)
		}

		aircraft = append(aircraft, target)

		// Note: OpenSky's on_ground is available, but ADSBTarget doesn't have a dedicated OnGround field.
		// The higher-level processing (Service.ProcessRawData) infers on-ground using speed/altitude, so we leave it to that logic.
		_ = onGround
	}

	data := &RawAircraftData{
		Now:      float64(osResp.Time),
		Messages: len(aircraft),
		Aircraft: aircraft,
	}

	c.logger.Debug("Successfully fetched OpenSky ADS-B data",
		logger.Int("aircraft_count", len(data.Aircraft)),
		logger.String("source", "opensky"),
	)

	return data, nil
}

// UpdateStationCoords updates the station coordinates used for external API calls
func (c *Client) UpdateStationCoords(lat, lon float64) {
	c.stationLat = lat
	c.stationLon = lon

	c.logger.Debug("Station coordinates updated",
		logger.Float64("latitude", lat),
		logger.Float64("longitude", lon))
}

// SetOpenSkyConfig allows updating OpenSky-specific configuration on the client at runtime.
func (c *Client) SetOpenSkyConfig(credsPath string, lamin, lomin, lamax, lomax float64) {
	if credsPath != "" {
		c.openskyCredsPath = credsPath
	}
	if !(lamin == 0 && lomin == 0 && lamax == 0 && lomax == 0) {
		c.openskyBBoxLamin = lamin
		c.openskyBBoxLomin = lomin
		c.openskyBBoxLamax = lamax
		c.openskyBBoxLomax = lomax
	}

	c.logger.Debug("OpenSky configuration updated",
		logger.String("creds_path", c.openskyCredsPath),
		logger.Float64("lamin", c.openskyBBoxLamin),
		logger.Float64("lomin", c.openskyBBoxLomin),
		logger.Float64("lamax", c.openskyBBoxLamax),
		logger.Float64("lomax", c.openskyBBoxLomax),
	)
}
