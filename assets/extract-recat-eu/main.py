import csv
import pdfplumber

def is_recat_value(val):
    if not val:
        return False
    val = val.upper().strip()
    if val.startswith("CAT-") or val in ["SPECIAL", "TBC", "NONE"]:
        return True
    return False

def is_legacy_value(val):
    if not val:
        return False
    val = val.upper().strip()
    if val in ["L", "M", "H", "J"]:
        return True
    return False

def pdf_to_csv(pdf_path, csv_path):
    with pdfplumber.open(pdf_path) as pdf:
        all_rows = []
        print(f"Processing {len(pdf.pages)} pages...")

        for page in pdf.pages:
            # 1. Extract words with coordinates
            words = page.extract_words(x_tolerance=3, y_tolerance=3)
            
            # 2. Cluster words into rows based on 'top' (Y-coordinate)
            rows = {}
            for w in words:
                y_bin = round(w['top'] / 3) * 3
                if y_bin not in rows:
                    rows[y_bin] = []
                rows[y_bin].append(w)
            
            sorted_y = sorted(rows.keys())
            for y in sorted_y:
                row_words = sorted(rows[y], key=lambda x: x['x0'])
                
                # 3. Absolute Column Buckets
                # Based on diagnostic:
                # Manuf ends < 190 (Eurocopter 141)
                # Model starts > 190 (Super Puma 197)
                # Designator starts > 300 (AS3B 304, AT8T > 300?)
                # Legacy starts > 370 (M 377)
                # RECAT starts > 420 (CAT-F 423)
                
                manuf_words = []
                model_words = []
                desig_words = []
                legacy_words = []
                recat_words = []
                
                for w in row_words:
                    x0 = w['x0']
                    text = w['text']
                    
                    if x0 < 190:
                        manuf_words.append(text)
                    elif 190 <= x0 < 295:  # Slightly relaxed upper bound for Model
                        model_words.append(text)
                    elif 295 <= x0 < 370:  # Designator usually starts ~304
                        desig_words.append(text)
                    elif 370 <= x0 < 410:  # Legacy starts ~377
                        legacy_words.append(text)
                    elif x0 >= 410:        # RECAT starts ~423
                        recat_words.append(text)
                        
                # Join cols
                manufacturer = " ".join(manuf_words)
                model = " ".join(model_words)
                designator = " ".join(desig_words)
                legacy_val = " ".join(legacy_words)
                recat_val = " ".join(recat_words)
                
                # Valid Row Filter
                # Check for header/footer keywords in full string
                row_str = f"{manufacturer} {model} {designator} {legacy_val} {recat_val}".upper()
                
                invalid_keywords = [
                    "EUROPEAN UNION", "SAFETY AGENCY", "PROPRIETARY", 
                    "RESERVED", "ISO9001", "TE.GEN", "PAGE OF", 
                    "DATA DESIGNATOR", "LEGACY WTC", "SIGNATURE",
                    "PREPARED", "REVIEWED", "STRATEGY", "PROGRAMME",
                    "MANUFACTURER"
                ]
                if any(k in row_str for k in invalid_keywords):
                    continue
                
                # We need reasonably populated rows.
                # Must have Manufacturer, Model, Designator.
                if not manufacturer or not model or not designator:
                    continue
                
                # Strict Data Validation:
                # Real aircraft rows MUST have a valid RECAT or Legacy WTC.
                # Garbage rows (like introductory text) will have random words like "Wake" or "Turbulence".
                if not (is_recat_value(recat_val) or is_legacy_value(legacy_val)):
                    continue

                all_rows.append([manufacturer, model, designator, legacy_val, recat_val])

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

if __name__ == "__main__":
    import sys
    input_pdf = "recat-eu.pdf"
    output_csv = "recat_eu_aircraft.csv"
    if len(sys.argv) > 2:
        input_pdf = sys.argv[1]
    if len(sys.argv) > 3:
        output_csv = sys.argv[2]
    pdf_to_csv(input_pdf, output_csv)
