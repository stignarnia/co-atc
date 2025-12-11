package weather

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/yegors/co-atc/internal/physics"
	"github.com/yegors/co-atc/pkg/logger"
)

// GFSClient handles fetching and storing GFS weather grids
type GFSClient struct {
	config     GFSConfig
	httpClient *http.Client
	logger     *logger.Logger

	// In-memory storage for the current weather grid
	grid *GFSGrid
	mu   sync.RWMutex
}

// GFSGrid represents a 4D weather grid (Time, Level, Lat, Lon)
type GFSGrid struct {
	LastUpdated time.Time
	Times       []time.Time // Sorted timestamps for the forecast
	Latitudes   []float64   // Sorted latitudes
	Longitudes  []float64   // Sorted longitudes
	Levels      []int       // Pressure levels in hPa (e.g., 1000, 950, ..., 150)

	// Data stored as [TimeIndex][LevelIndex][LatIndex][LonIndex]
	UWind [][][][]float64 // U-component of wind (m/s)
	VWind [][][][]float64 // V-component of wind (m/s)
	Temp  [][][][]float64 // Temperature (Celsius)
}

// NewGFSClient creates a new GFS client
func NewGFSClient(config GFSConfig, logger *logger.Logger) *GFSClient {
	return &GFSClient{
		config: config,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		logger: logger.Named("gfs-client"),
	}
}

// FetchRegionalGrid fetches a 3D weather grid centered on the given coordinates
func (c *GFSClient) FetchRegionalGrid(centerLat, centerLon float64) error {
	if !c.config.Enabled {
		return nil
	}

	// 1. Define Grid Points
	// We use a 3x3 grid centered on the station.
	// Step size derived from configured radius.
	radiusNM := c.config.GridDomainRadiusNM
	if radiusNM <= 0 {
		radiusNM = 50.0 // Default fallback
	}
	gridStep := radiusNM / 50.0 // Convert NM to degrees (approx)

	lats := []float64{centerLat - gridStep, centerLat, centerLat + gridStep}
	lons := []float64{centerLon - gridStep, centerLon, centerLon + gridStep}

	// Construct parallel lists for the API request (9 points)
	// (lat0,lon0), (lat0,lon1), (lat0,lon2), (lat1,lon0)...
	var reqLats []string
	var reqLons []string
	for _, lat := range lats {
		for _, lon := range lons {
			reqLats = append(reqLats, fmt.Sprintf("%f", lat))
			reqLons = append(reqLons, fmt.Sprintf("%f", lon))
		}
	}

	latStr := ""
	lonStr := ""
	for i := 0; i < len(reqLats); i++ {
		latStr += reqLats[i] + ","
		lonStr += reqLons[i] + ","
	}
	latStr = latStr[:len(latStr)-1] // Remove trailing comma
	lonStr = lonStr[:len(lonStr)-1]

	// 2. Define Pressure Levels
	levels := []int{1000, 950, 925, 900, 850, 800, 700, 600, 500, 400, 300, 250, 200, 150}

	// 3. Construct Variable List
	reqLevels := make([]string, len(levels))
	for i, l := range levels {
		reqLevels[i] = fmt.Sprintf("%dhPa", l)
	}

	// variables: temperature_1000hPa, windspeed_1000hPa, winddirection_1000hPa
	paramStr := ""
	for _, lvl := range reqLevels {
		paramStr += fmt.Sprintf("temperature_%s,windspeed_%s,winddirection_%s,", lvl, lvl, lvl)
	}
	paramStr = paramStr[:len(paramStr)-1]

	// 4. Execute API Request
	url := fmt.Sprintf("%s?latitude=%s&longitude=%s&hourly=%s&wind_speed_unit=ms&timezone=UTC&forecast_days=1",
		c.config.BaseURL, latStr, lonStr, paramStr)

	c.logger.Info("Fetching GFS regional grid",
		logger.Float64("center_lat", centerLat),
		logger.Float64("center_lon", centerLon),
		logger.Int("points", 9))

	resp, err := c.httpClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Read body for error details
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GFS API returned status: %d, body: %s", resp.StatusCode, string(body))
	}

	// 5. Parse Response
	// Open-Meteo returns a JSON Array of objects for multi-point requests
	var results []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return err
	}

	if len(results) != 9 {
		return fmt.Errorf("expected 9 grid points, got %d", len(results))
	}

	// 6. Populate Grid Structure
	c.parseGridResponse(results, lats, lons, levels)
	return nil
}

// parseGridResponse parses the array of results into the 4D GFSGrid
func (c *GFSClient) parseGridResponse(results []map[string]interface{}, lats, lons []float64, levels []int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Extract times from the first result (assuming they are consistent across all points)
	var times []time.Time
	if len(results) > 0 {
		hourly, ok := results[0]["hourly"].(map[string]interface{})
		if ok {
			if timeStrs, ok := hourly["time"].([]interface{}); ok {
				for _, t := range timeStrs {
					if ts, ok := t.(string); ok {
						// Open-Meteo format: "2023-12-09T14:00"
						parsedTime, err := time.Parse("2006-01-02T15:04", ts)
						if err == nil {
							times = append(times, parsedTime)
						}
					}
				}
			}
		}
	}

	if len(times) == 0 {
		c.logger.Error("Failed to parse forecast times from GFS response")
		return
	}

	timeCount := len(times)
	levelCount := len(levels)
	latCount := len(lats)
	lonCount := len(lons)

	grid := &GFSGrid{
		LastUpdated: time.Now(),
		Times:       times,
		Latitudes:   lats,
		Longitudes:  lons,
		Levels:      levels,
		UWind:       make([][][][]float64, timeCount),
		VWind:       make([][][][]float64, timeCount),
		Temp:        make([][][][]float64, timeCount),
	}

	// Initialize 4D arrays
	for t := 0; t < timeCount; t++ {
		grid.UWind[t] = make([][][]float64, levelCount)
		grid.VWind[t] = make([][][]float64, levelCount)
		grid.Temp[t] = make([][][]float64, levelCount)
		for l := 0; l < levelCount; l++ {
			grid.UWind[t][l] = make([][]float64, latCount)
			grid.VWind[t][l] = make([][]float64, latCount)
			grid.Temp[t][l] = make([][]float64, latCount)
			for lat := 0; lat < latCount; lat++ {
				grid.UWind[t][l][lat] = make([]float64, lonCount)
				grid.VWind[t][l][lat] = make([]float64, lonCount)
				grid.Temp[t][l][lat] = make([]float64, lonCount)
			}
		}
	}

	// Results are flat list of 9 points. Order matches request:
	// (lat0, lon0), (lat0, lon1), (lat0, lon2) -> latIndex=0, lonIndex=0,1,2
	// (lat1, lon0), ...
	resultIdx := 0

	for latIdx := range lats {
		for lonIdx := range lons {
			data := results[resultIdx]
			hourly, ok := data["hourly"].(map[string]interface{})
			if ok {
				for t := 0; t < timeCount; t++ {
					for lvlIdx, lvl := range levels {
						suffix := fmt.Sprintf("%dhPa", lvl)

						// Parse and store values
						ws := extractValue(hourly, "windspeed_"+suffix, t)
						wd := extractValue(hourly, "winddirection_"+suffix, t)
						temp := extractValue(hourly, "temperature_"+suffix, t)

						// Convert Speed/Dir to U/V
						// U = -ws * sin(wd * pi/180)
						// V = -ws * cos(wd * pi/180)
						rad := wd * math.Pi / 180.0
						u := -ws * math.Sin(rad)
						v := -ws * math.Cos(rad)

						grid.UWind[t][lvlIdx][latIdx][lonIdx] = u
						grid.VWind[t][lvlIdx][latIdx][lonIdx] = v
						grid.Temp[t][lvlIdx][latIdx][lonIdx] = temp
					}
				}
			}
			resultIdx++
		}
	}

	c.grid = grid
	c.logger.Info("GFS Data Updated",
		logger.Int("times", timeCount),
		logger.Int("levels", levelCount),
		logger.Int("lat_points", latCount),
		logger.Int("lon_points", lonCount),
		logger.String("time_range", fmt.Sprintf("%s to %s", times[0], times[len(times)-1])),
	)
}

func extractValue(hourly map[string]interface{}, key string, idx int) float64 {
	if arr, ok := hourly[key].([]interface{}); ok && len(arr) > idx {
		if val, ok := arr[idx].(float64); ok {
			return val
		}
	}
	return 0.0 // Default or Error value
}

// GetConditions returns the interpolated wind and temperature for a given 3D position at the current time
func (c *GFSClient) GetConditions(lat, lon, altFt float64) (u, v, temp float64, err error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.grid == nil {
		return 0, 0, 0, fmt.Errorf("no GFS data available")
	}

	// 1. Find Time Interpolation Factors
	now := time.Now().UTC()
	times := c.grid.Times
	if len(times) == 0 {
		return 0, 0, 0, fmt.Errorf("empty GFS time data")
	}

	// Find the time bracket [t1, t2] such that t1 <= now <= t2
	idx1 := -1
	idx2 := -1

	// If now is before the first time, clamp to first time
	if now.Before(times[0]) {
		idx1, idx2 = 0, 0
	} else if now.After(times[len(times)-1]) {
		// If now is after the last time, clamp to last time
		last := len(times) - 1
		idx1, idx2 = last, last
	} else {
		// Linear scan to find bracket
		for i := 0; i < len(times)-1; i++ {
			if !now.Before(times[i]) && !now.After(times[i+1]) {
				idx1 = i
				idx2 = i + 1
				break
			}
		}
	}

	// Fallback if binary search or logic missed (shouldn't happen given above checks)
	if idx1 == -1 {
		idx1 = 0
		idx2 = 0
	}

	// Calculate time interpolation factor (alpha)
	// alpha = (now - t1) / (t2 - t1)
	alpha := 0.0
	if idx1 != idx2 {
		t1 := times[idx1]
		t2 := times[idx2]
		duration := t2.Sub(t1).Seconds()
		if duration > 0 {
			elapsed := now.Sub(t1).Seconds()
			alpha = elapsed / duration
		}
	}

	// 2. Spatially Interpolate for t1 and t2
	u1, v1, temp1, err1 := c.getSpatialConditionsAtTime(idx1, lat, lon, altFt)
	if err1 != nil {
		return 0, 0, 0, err1
	}

	// Optimization: if we are exactly on a time step or clamped
	if idx1 == idx2 || alpha == 0.0 {
		return u1, v1, temp1, nil
	}

	u2, v2, temp2, err2 := c.getSpatialConditionsAtTime(idx2, lat, lon, altFt)
	if err2 != nil {
		return 0, 0, 0, err2
	}

	// 3. Temporal Interpolation
	u = u1*(1-alpha) + u2*alpha
	v = v1*(1-alpha) + v2*alpha
	temp = temp1*(1-alpha) + temp2*alpha

	// Debug log occasionally (optional, kept light)
	// c.logger.Debug("Interpolated weather", logger.Float64("alpha", alpha), logger.String("t1", times[idx1].String()), logger.String("t2", times[idx2].String()))

	return u, v, temp, nil
}

// getSpatialConditionsAtTime performs 3D spatial interpolation for a fixed time index
func (c *GFSClient) getSpatialConditionsAtTime(timeIdx int, lat, lon, altFt float64) (u, v, temp float64, err error) {
	// 1. Bilinear Interpolation at Bounding Pressure Levels
	// Find pressure level bounds for the given altitude
	targetPressure := physics.AltitudeToPressure(altFt)

	lowerLvlIdx, upperLvlIdx := -1, -1

	// Scan for bounding pressure levels
	for i := 0; i < len(c.grid.Levels)-1; i++ {
		p1 := float64(c.grid.Levels[i])   // Higher pressure (lower alt)
		p2 := float64(c.grid.Levels[i+1]) // Lower pressure (higher alt)
		if targetPressure <= p1 && targetPressure >= p2 {
			lowerLvlIdx = i
			upperLvlIdx = i + 1
			break
		}
	}

	// Handle altitude out of bounds (clamp to surface or ceiling)
	if targetPressure > float64(c.grid.Levels[0]) {
		lowerLvlIdx, upperLvlIdx = 0, 0 // Use surface
	} else if targetPressure < float64(c.grid.Levels[len(c.grid.Levels)-1]) {
		last := len(c.grid.Levels) - 1
		lowerLvlIdx, upperLvlIdx = last, last
	}

	// Helper to interpolate 2D grid at a specific level index
	interpolateLevel := func(lvlIdx int) (u, v, t float64) {
		return c.bilinearInterpolate(timeIdx, lvlIdx, lat, lon)
	}

	u1, v1, t1 := interpolateLevel(lowerLvlIdx)

	if lowerLvlIdx == upperLvlIdx {
		return u1, v1, t1, nil
	}

	u2, v2, t2 := interpolateLevel(upperLvlIdx)

	// Linear interpolation vertically
	p1 := float64(c.grid.Levels[lowerLvlIdx])
	p2 := float64(c.grid.Levels[upperLvlIdx])
	ratio := (p1 - targetPressure) / (p1 - p2)

	u = u1 + (u2-u1)*ratio
	v = v1 + (v2-v1)*ratio
	temp = t1 + (t2-t1)*ratio

	return u, v, temp, nil
}

// bilinearInterpolate performs 2D interpolation for a specific time and level
func (c *GFSClient) bilinearInterpolate(timeIdx, lvlIdx int, lat, lon float64) (u, v, t float64) {
	// Find bounding lat/lon indices
	// Lats/Lons are sorted ascending

	latIdx1, latIdx2 := findBounds(c.grid.Latitudes, lat)
	lonIdx1, lonIdx2 := findBounds(c.grid.Longitudes, lon)

	// Get 4 points
	// Q11 (lat1, lon1), Q12 (lat1, lon2), Q21 (lat2, lon1), Q22 (lat2, lon2)

	// Helper to get values
	getVal := func(li, gi int) (float64, float64, float64) {
		return c.grid.UWind[timeIdx][lvlIdx][li][gi], c.grid.VWind[timeIdx][lvlIdx][li][gi], c.grid.Temp[timeIdx][lvlIdx][li][gi]
	}

	u11, v11, t11 := getVal(latIdx1, lonIdx1)
	u12, v12, t12 := getVal(latIdx1, lonIdx2)
	u21, v21, t21 := getVal(latIdx2, lonIdx1)
	u22, v22, t22 := getVal(latIdx2, lonIdx2)

	// Interpolation fractions
	lat1 := c.grid.Latitudes[latIdx1]
	lat2 := c.grid.Latitudes[latIdx2]
	lon1 := c.grid.Longitudes[lonIdx1]
	lon2 := c.grid.Longitudes[lonIdx2]

	var tLat, tLon float64
	if lat2 != lat1 {
		tLat = (lat - lat1) / (lat2 - lat1)
	}
	if lon2 != lon1 {
		tLon = (lon - lon1) / (lon2 - lon1)
	}

	// Interpolate
	// x = tLon, y = tLat

	interp := func(v11, v12, v21, v22 float64) float64 {
		return v11*(1-tLon)*(1-tLat) + v21*(1-tLon)*tLat + v12*tLon*(1-tLat) + v22*tLon*tLat
	}

	u = interp(u11, u12, u21, u22)
	v = interp(v11, v12, v21, v22)
	t = interp(t11, t12, t21, t22)

	return u, v, t
}

func findBounds(arr []float64, val float64) (int, int) {
	// Simple scan since small array
	last := len(arr) - 1
	if val <= arr[0] {
		return 0, 0
	}
	if val >= arr[last] {
		return last, last
	}
	for i := 0; i < last; i++ {
		if val >= arr[i] && val <= arr[i+1] {
			return i, i + 1
		}
	}
	return 0, 0 // Should not happen
}
