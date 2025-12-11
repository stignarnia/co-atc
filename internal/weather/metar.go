package weather

// METARResponse represents the JSON response from https://aviationweather.gov/api/data/metar?ids={CODE}&format=json
// The API returns an array []METARResponse
type METARResponse struct {
	ICAOId     string  `json:"icaoId"`
	ReportTime string  `json:"reportTime"` // "2025-12-10T01:00:00.000Z"
	Temp       float64 `json:"temp"`       // Temperature in Celsius
	Dewp       float64 `json:"dewp"`       // Dewpoint in Celsius
	Wdir       float64 `json:"wdir"`       // Wind direction in degrees
	Wspd       float64 `json:"wspd"`       // Wind speed in knots (kt)
	Visib      any     `json:"visib"`      // Visibility (can be string "10+" or number 4.97)
	Altim      float64 `json:"altim"`      // Altimeter setting in hPa (e.g. 1017.4) - Wait, US is usually Hg?
	// Sample says "altim": 1017.4. KJFK A3004 is 30.04 inHg = 1017.27 hPa.
	// So this is likely hPa.
	RawOb  string  `json:"rawOb"` // Raw METAR string
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	Elev   float64 `json:"elev"`
	Name   string  `json:"name"`
	FltCat string  `json:"fltCat"` // VFR, MVFR, IFR, LIFR
}

// TAFResponse represents the JSON response from https://aviationweather.gov/api/data/taf?ids={CODE}&format=json
// The API returns an array []TAFResponse
type TAFResponse struct {
	ICAOId    string `json:"icaoId"`
	IssueTime string `json:"issueTime"`
	ValidTime string `json:"validTime"` // Not exact field name, check sample but RawTAF is what we need
	RawTAF    string `json:"rawTAF"`
	Remarks   string `json:"remarks"`
}
