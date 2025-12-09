package physics

import (
	"math"
	"time"

	"github.com/westphae/geomag/pkg/egm96"
	"github.com/westphae/geomag/pkg/wmm"
)

// Constants
const (
	R           = 287.058  // Specific gas constant for dry air (J/(kg·K))
	Gamma       = 1.4      // Adiabatic index (heat capacity ratio)
	G           = 9.80665  // Gravity (m/s^2)
	T0          = 288.15   // Standard Sea Level Temperature (K)
	P0          = 1013.25  // Standard Sea Level Pressure (hPa)
	L           = 0.0065   // Temperature Lapse Rate (K/m) in Troposphere
	ZeroCelsius = 273.15   // 0°C in Kelvin
	KnotsToMs   = 0.514444 // Conversion factor from Knots to m/s
	MsToKnots   = 1.94384  // Conversion factor from m/s to Knots

	// ISA Layer Boundaries
	TropopauseAltM    = 11000.0 // 11 km
	TropopauseAltFt   = 36089.2 // ~36,089 ft
	StratosphereTempK = 216.65  // Constant temperature in Stratosphere
	TropopausePress   = 226.32  // Pressure at Tropopause (hPa)
)

// CalculateSoundSpeed returns the speed of sound in m/s for a given temperature in Kelvin
func CalculateSoundSpeed(tempK float64) float64 {
	if tempK <= 0 {
		return 0
	}
	return math.Sqrt(Gamma * R * tempK)
}

// CalculateMach returns the Mach number given TAS (knots) and Temperature (Celsius)
func CalculateMach(tasKnots float64, tempCelsius float64) float64 {
	tempK := tempCelsius + ZeroCelsius
	a := CalculateSoundSpeed(tempK)
	if a == 0 {
		return 0
	}

	tasMs := tasKnots * KnotsToMs
	return tasMs / a
}

// CalculateTAT returns Total Air Temperature (Celsius) given OAT (Celsius) and Mach number
// Formula: TAT_K = OAT_K * (1 + 0.2 * M^2)
func CalculateTAT(oatCelsius float64, mach float64) float64 {
	oatK := oatCelsius + ZeroCelsius
	tatK := oatK * (1.0 + 0.2*mach*mach)
	return tatK - ZeroCelsius
}

// CalculateTASFromMach returns True Airspeed in knots given Mach and Temperature (Celsius)
func CalculateTASFromMach(mach float64, tempCelsius float64) float64 {
	tempK := tempCelsius + ZeroCelsius
	a := CalculateSoundSpeed(tempK)

	tasMs := mach * a
	return tasMs * MsToKnots
}

// CalculateCAS calculates Calibrated Airspeed (CAS) using compressible flow equations (Saint-Venant).
// This is more accurate than simple IAS/EAS formulas for Mach > 0.3.
// Inputs: TAS (knots), Pressure Altitude (ft), Temperature (Celsius)
func CalculateCAS(tasKnots float64, pressAltFt float64, tempCelsius float64) float64 {
	// 1. Get Static Pressure (P) at altitude
	pHPa := AltitudeToPressure(pressAltFt)
	pPa := pHPa * 100.0 // Convert hPa to Pa
	p0Pa := P0 * 100.0  // Standard Sea Level Pressure (Pa)

	// 2. Calculate Mach Number
	mach := CalculateMach(tasKnots, tempCelsius)

	// 3. Saint-Venant Equation for Impact Pressure (qc)
	// qc = P * [ (1 + 0.2 * M^2)^3.5 - 1 ]
	qc := pPa * (math.Pow(1+0.2*mach*mach, 3.5) - 1)

	// 4. Calculate CAS from Impact Pressure
	// CAS = a0 * sqrt( 5 * [ (qc/P0 + 1)^(1/3.5) - 1 ] )
	// a0 = Standard Speed of Sound at Sea Level (approx 661.47 knots)
	a0 := CalculateSoundSpeed(T0) * MsToKnots

	term := (qc / p0Pa) + 1
	if term < 0 {
		return 0
	}

	cas := a0 * math.Sqrt(5*(math.Pow(term, 1/3.5)-1))
	return cas
}

// CalculateDensityAltitude returns density altitude in feet
func CalculateDensityAltitude(pressureAltFt float64, tempCelsius float64) float64 {
	// ISA Temp at pressure altitude
	isaTempK := T0 - (L * (pressureAltFt * 0.3048))
	if pressureAltFt > TropopauseAltFt {
		isaTempK = StratosphereTempK
	}
	isaTempC := isaTempK - ZeroCelsius

	// DA = PA + 120 * (OAT - ISA_Temp)
	return pressureAltFt + 120*(tempCelsius-isaTempC)
}

// ------------------------------------------------------------------------------------------------
// NAVIGATION PHYSICS
// ------------------------------------------------------------------------------------------------

// Vector2D represents a 2D vector (magnitude, direction)
type Vector2D struct {
	X float64 // East component
	Y float64 // North component
}

// HeadingToVector converts a heading (degrees) and magnitude to X/Y components
func HeadingToVector(headingDeg float64, magnitude float64) Vector2D {
	rad := (90 - headingDeg) * math.Pi / 180 // Convert compass heading to math angle
	return Vector2D{
		X: magnitude * math.Cos(rad),
		Y: magnitude * math.Sin(rad),
	}
}

// AltitudeToPressure converts pressure altitude in feet to pressure in hPa
// Uses Standard Atmosphere model, supporting Troposphere and Stratosphere (up to 20km approx)
func AltitudeToPressure(altFt float64) float64 {
	altM := altFt * 0.3048
	if altM < 0 {
		altM = 0
	}

	if altM <= TropopauseAltM {
		// Troposphere Model (0 - 11km)
		// P = P0 * (1 - L*h/T0)^(g/RL)
		exponent := (G) / (R * L)
		base := 1 - (L * altM / T0)
		return P0 * math.Pow(base, exponent)
	} else {
		// Stratosphere Model (> 11km)
		// P = P_trop * exp( -g*(h - h_trop) / (R * T_strat) )
		relAlt := altM - TropopauseAltM
		exponent := -(G * relAlt) / (R * StratosphereTempK)
		return TropopausePress * math.Exp(exponent)
	}
}

// SolveWindTriangle calculates TAS and True Heading given Ground Speed, Track, and Wind Vector
// gs: Ground Speed (knots)
// track: Track (degrees)
// windU: Wind U component (m/s) (+East)
// windV: Wind V component (m/s) (+North)
// Returns: tas (knots), trueHeading (degrees)
func SolveWindTriangle(gsKnots float64, trackDeg float64, windU_Ms float64, windV_Ms float64) (float64, float64) {
	// 1. Convert everything to consistent units (Knots and Vectors)
	windU_Kts := windU_Ms * MsToKnots
	windV_Kts := windV_Ms * MsToKnots

	// Ground Vector (what we observe)
	groundVec := HeadingToVector(trackDeg, gsKnots)

	// Wind Vector (what the air is doing)
	// Wind U is East (X), Wind V is North (Y)
	windVec := Vector2D{X: windU_Kts, Y: windV_Kts}

	// Air Mass Vector (Velocity of aircraft relative to air)
	// V_ground = V_air + V_wind
	// => V_air = V_ground - V_wind

	airVecX := groundVec.X - windVec.X
	airVecY := groundVec.Y - windVec.Y

	// Calculate TAS (Magnitude of Air Vector)
	tas := math.Sqrt(airVecX*airVecX + airVecY*airVecY)

	// Calculate True Heading (Direction of Air Vector)
	// math.Atan2 returns angle from X axis (East) in radians
	rad := math.Atan2(airVecY, airVecX)

	// Convert math angle to compass heading
	// Compass = 90 - MathDegree
	heading := 90 - (rad * 180 / math.Pi)

	// Normalize to 0-360
	if heading < 0 {
		heading += 360
	}
	if heading >= 360 {
		heading -= 360
	}

	return tas, heading
}

// CalculateMagneticVariation calculates the magnetic declination for a given position and time
// Returns declination in degrees (+East, -West)
func CalculateMagneticVariation(lat, lon, altFt float64, date time.Time) float64 {
	// Convert altitude to meters for WMM
	altM := altFt * 0.3048

	// Create location from Geodetic coordinates
	loc := egm96.NewLocationGeodetic(lat, lon, altM)

	// Calculate magnetic field
	mag, err := wmm.CalculateWMMMagneticField(loc, date)
	if err != nil {
		// Return 0 for safety if calculation fails
		return 0.0
	}

	return mag.D() // Declination
}
