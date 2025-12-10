package weather

import (
	"regexp"
	"strconv"
	"strings"
)

// WindyMETARResponse Represents the JSON response from https://node.windy.com/airports/metar/{CODE}
// Matches the structure: { "source": "Internal", "trend": [ ... ] }
type WindyMETARResponse struct {
	Note   string       `json:"note,omitempty"`
	Source string       `json:"source"`
	Trend  []WindyMETAR `json:"trend"`
}

// WindyMETAR represents a single METAR observation in the "trend" list
type WindyMETAR struct {
	MetarRaw string    `json:"metar"` // "KJFK 092251Z ..."
	Ux       int64     `json:"ux"`
	Type     string    `json:"type"`
	Txt      []string  `json:"txt"`
	Rmk      string    `json:"rmk"`
	Wind     WindyWind `json:"wind"`
}

// WindyWind represents the wind object in the METAR response
type WindyWind struct {
	Dir      string  `json:"dir"` // API returns this as string "220"
	Speed    float64 `json:"speed"`
	Measure  string  `json:"measure"`
	SpeedMPS float64 `json:"speedMPS"`
}

// ParseTemperature extracts the temperature in Celsius from the raw METAR string.
// Standard Format: "22/M05" (22°C, Dewpoint -5°C) or "M02/M10" (-2°C / -10°C)
// Also supports RMK T-group: "T00561050" (Precise Temp: 5.6°C)
func (m *WindyMETAR) ParseTemperature() (float64, bool) {
	// 1. Try High Precision "T-Group" in Remarks (RMK)
	// Format: T00561050 -> T s ttt s ddd (s=sign 0=pos,1=neg; ttt=temp*10)
	// Regex: T([01])(\d{3})
	if strings.Contains(m.MetarRaw, "RMK") {
		reTGroup := regexp.MustCompile(`T([01])(\d{3})`)
		matches := reTGroup.FindStringSubmatch(m.MetarRaw)
		if len(matches) == 3 {
			sign := matches[1] // "0" or "1"
			tempRaw := matches[2]

			val, err := strconv.ParseFloat(tempRaw, 64)
			if err == nil {
				val = val / 10.0
				if sign == "1" {
					val = -val
				}
				return val, true
			}
		}
	}

	// 2. Standard Temperature/Dewpoint Group
	// Regex matches: Space, (M or nothing)(\d{2}) / ...
	// Examples: " 22/10", " M03/M05", " 00/M01"
	reStandard := regexp.MustCompile(`\s(M)?(\d{2})/(?:M)?\d{2}`)
	matches := reStandard.FindStringSubmatch(m.MetarRaw)
	if len(matches) == 3 {
		isMinus := matches[1] == "M"
		tempRaw := matches[2]

		val, err := strconv.ParseFloat(tempRaw, 64)
		if err == nil {
			if isMinus {
				val = -val
			}
			return val, true
		}
	}

	return 0, false
}
