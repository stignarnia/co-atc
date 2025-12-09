package physics

import (
	"math"
)

// Constants
const (
	R           = 287.058  // Specific gas constant for dry air (J/(kg·K))
	Gamma       = 1.4      // Adiabatic index (heat capacity ratio)
	G           = 9.80665  // Gravity (m/s^2)
	T0          = 288.15   // Standard Sea Level Temperature (K)
	P0          = 1013.25  // Standard Sea Level Pressure (hPa)
	L           = 0.0065   // Temperature Lapse Rate (K/m)
	ZeroCelsius = 273.15   // 0°C in Kelvin
	KnotsToMs   = 0.514444 // Conversion factor from Knots to m/s
	MsToKnots   = 1.94384  // Conversion factor from m/s to Knots
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

// CalculateTASFromMach returns True Airspeed in knots given Mach and Temperature (Celsius)
func CalculateTASFromMach(mach float64, tempCelsius float64) float64 {
	tempK := tempCelsius + ZeroCelsius
	a := CalculateSoundSpeed(tempK)

	tasMs := mach * a
	return tasMs * MsToKnots
}

// CalculateIASWithTemp calculates IAS (knots) from TAS (knots), Pressure (hPa), and Temp (Celsius)
func CalculateIASWithTemp(tasKnots float64, pressureHPa float64, tempCelsius float64) float64 {
	// rho = P / (R * T)
	// P in Pa, T in K

	pPa := pressureHPa * 100
	tK := tempCelsius + ZeroCelsius
	rho := pPa / (R * tK)

	// Sea level density rho0 = 1.225 kg/m^3
	rho0 := 1.225

	sigma := rho / rho0
	if sigma <= 0 {
		return 0
	}

	return tasKnots * math.Sqrt(sigma)
}

// CalculateDensityAltitude returns density altitude in feet
func CalculateDensityAltitude(pressureAltFt float64, tempCelsius float64) float64 {
	// ISA Temp at pressure altitude
	isaTempK := T0 - (L * (pressureAltFt * 0.3048))
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

// AltitudeToPressure converts pressure altitude in feet to pressure in hPa (Standard Atmosphere)
func AltitudeToPressure(altFt float64) float64 {
	if altFt < 0 {
		altFt = 0
	}
	// P = P0 * (1 - L*h/T0)^(gM/RL)
	// P0 = 1013.25
	// exponent constant ~ 5.25588
	return P0 * math.Pow(1-(L*(altFt*0.3048)/T0), (G*0.0289644)/(R*L))
}

// SolveWindTriangle calculates TAS and True Heading given Ground Speed, Track, and Wind Vector
// gs: Ground Speed (knots)
// track: Track (degrees)
// windU: Wind U component (m/s) (+East)
// windV: Wind V component (m/s) (+North)
// Returns: tas (knots), heading (degrees)
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
