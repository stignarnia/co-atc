import csv

import pdfplumber


def is_recat_value(val):
    """
    Determines if a value looks like a RECAT-EU code.
    RECAT codes are typically CAT-A, CAT-B, etc., or 'Special', 'TBC'.
    """
    if not val:
        return False
    val = val.upper().strip()
    # Matches CAT-A through CAT-F, or Special, or TBC
    if val.startswith("CAT-") or val in ["SPECIAL", "TBC", "NONE"]:
        return True
    return False


def is_legacy_value(val):
    """
    Determines if a value looks like a Legacy WTC code.
    Legacy codes are typically L, M, H, or J (Super).
    """
    if not val:
        return False
    val = val.upper().strip()
    # Strict matching for Legacy usually H, M, L. J is sometimes used for A380.
    if val in ["L", "M", "H", "J"]:
        return True
    return False


def clean_text(text):
    """Removes newlines and extra spaces."""
    if text:
        return " ".join(text.split()).strip()
    return ""


def pdf_to_csv(pdf_path, csv_path):
    with pdfplumber.open(pdf_path) as pdf:
        all_rows = []

        print(f"Processing {len(pdf.pages)} pages...")

        for i, page in enumerate(pdf.pages):
            # Extract table. pdfplumber attempts to identify table structures.
            # We use strict tolerance to avoid merging columns accidentally.
            tables = page.extract_tables()

            for table in tables:
                for row in table:
                    # Clean up the row data
                    clean_row = [clean_text(cell) for cell in row]

                    # Skip empty rows or rows with too few columns
                    # We expect at least Manufacturer, Model, Designator, WTC1, WTC2
                    if len(clean_row) < 4:
                        continue

                    # Header detection: Skip rows containing header keywords
                    if (
                        "MANUFACTURER" in clean_row[0].upper()
                        or "RECAT-EU" in "".join(clean_row).upper()
                    ):
                        continue

                    # Data Extraction Logic
                    # The table structure usually has 5 columns, but sometimes empty columns appear.
                    # We look for the "Designator" (usually 3rd or 4th substantial column)
                    # and the last two columns for WTC data.

                    # Remove empty columns (None or "") to normalize list
                    data = [x for x in clean_row if x]

                    if len(data) < 3:
                        continue

                    # Heuristic: The last two items are likely the WTC categories.
                    # The items before that are Manufacturer, Model, Designator.

                    wtc_candidate_1 = data[-2]
                    wtc_candidate_2 = data[-1]

                    # Logic to swap/assign columns
                    recat_val = "Unknown"
                    legacy_val = "Unknown"

                    # Check Case 1: Col 1 is RECAT (e.g., CAT-F), Col 2 is Legacy (e.g., L)
                    if is_recat_value(wtc_candidate_1) and is_legacy_value(
                        wtc_candidate_2
                    ):
                        recat_val = wtc_candidate_1
                        legacy_val = wtc_candidate_2

                    # Check Case 2: Col 1 is Legacy (e.g., L), Col 2 is RECAT (e.g., CAT-F)
                    elif is_legacy_value(wtc_candidate_1) and is_recat_value(
                        wtc_candidate_2
                    ):
                        legacy_val = wtc_candidate_1
                        recat_val = wtc_candidate_2

                    # Check Case 3: Edge cases (e.g., both TBC, or TBC and None)
                    # We prioritize identifying the explicit "CAT-" string
                    elif "CAT-" in wtc_candidate_1:
                        recat_val = wtc_candidate_1
                        legacy_val = wtc_candidate_2
                    elif "CAT-" in wtc_candidate_2:
                        recat_val = wtc_candidate_2
                        legacy_val = wtc_candidate_1
                    else:
                        # Fallback: Assume the order based on page heuristic or raw data observation
                        # If we can't decide, we leave them raw but flag them.
                        # Usually, if one is 'L'/'M'/'H', it's Legacy.
                        if is_legacy_value(wtc_candidate_1):
                            legacy_val = wtc_candidate_1
                            recat_val = wtc_candidate_2
                        elif is_legacy_value(wtc_candidate_2):
                            legacy_val = wtc_candidate_2
                            recat_val = wtc_candidate_1
                        else:
                            # If neither looks standard, just map 1->Legacy, 2->Recat (or vice versa)
                            # This catches weird rows.
                            legacy_val = wtc_candidate_1 + "?"
                            recat_val = wtc_candidate_2 + "?"

                    # Manufacturer, Model, Designator
                    # We assume Designator is always the 3rd item from the left (index 2)
                    # OR the item preceding the WTC codes.

                    designator = data[-3]

                    # Model is everything between Manufacturer and Designator
                    manufacturer = data[0]
                    model = " ".join(data[1:-3]) if len(data) > 4 else data[1]

                    all_rows.append(
                        [manufacturer, model, designator, legacy_val, recat_val]
                    )

        # Write to CSV
        with open(csv_path, "w", newline="", encoding="utf-8") as f:
            writer = csv.writer(f)
            writer.writerow(
                [
                    "Manufacturer",
                    "Model",
                    "ICAO Type Designator",
                    "ICAO Legacy WTC",
                    "RECAT-EU WTC",
                ]
            )
            writer.writerows(all_rows)

        print(f"Successfully converted {len(all_rows)} aircraft entries to {csv_path}")


# Run the function
input_pdf = "recat-eu.pdf"
output_csv = "recat_eu_aircraft.csv"
pdf_to_csv(input_pdf, output_csv)
