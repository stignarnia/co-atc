# Static Assets

This directory contains static data files used by Co-ATC for metadata enrichment and AI context.

## Data Sources

### `aircraft.csv`
Database of aircraft metadata (registration, type code, manufacturer, etc.) used to enrich live ADS-B data, particularly for sources that do not provide full metadata (e.g., OpenSky).

*   **Source**: [wiedehopf/tar1090-db](https://github.com/wiedehopf/tar1090-db)
*   **Format**: Semicolon-separated CSV (Hex;Registration;Type;...)

### `airlines.dat`
Database of airline operators including ICAO/IATA codes, callsigns, and country of origin. used for flight number matching and operator identification.

*   **Source**: [OpenFlights.org](https://openflights.org/data)
*   **Format**: CSV (ID, Name, Alias, IATA, ICAO, Callsign, Country, Active)

### `runways.csv`
Runway threshold coordinates for supported airports, used for accurate landing and takeoff detection relative to specific runways.

*   **Source**: [OurAirports.com](https://ourairports.com/data/)
*   **Format**: CSV (id, airport_ident, le_ident, le_latitude_deg, le_longitude_deg, ...)