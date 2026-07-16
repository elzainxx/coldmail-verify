#!/usr/bin/env bash
# Generate Docs/config-reference.md FROM Docs/config-manifest.md, so "what's toggleable"
# is always current and derived (never hand-drifted). Checked in; regenerated on manifest
# change; CI verifies it is up to date (docs-updated discipline, PROJECT_RULES.md §8).
#
# The manifest's knob tables all share the 6-column shape:
#   | name | type | default | <effect> | blast radius | source |
# This script extracts every data row (lines starting "| **"), classifies by `type`
# (secret | feature-flag | dial), and emits a compact, phase-agnostic reference + counts.
#
# Pure bash/awk (no toolchain beyond coreutils) so CI can run it anywhere.

set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MANIFEST="$HERE/Docs/config-manifest.md"
OUT="$HERE/Docs/config-reference.md"

[ -f "$MANIFEST" ] || { echo "gen-config-reference: manifest not found: $MANIFEST" >&2; exit 1; }

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

awk '
  BEGIN {
    FS="|"; total=0; sec=0; ff=0; dial=0; other=0;
  }
  # Track the current phase section from "## N. PHASE X ..." headers.
  /^## / {
    line=$0; sub(/^## /,"",line); phase=line; next
  }
  # Data rows: start with "| **name**". Skip header/separator rows.
  /^\| \*\*/ {
    # FS="|" and a trailing "|" makes $NF the empty field after the last cell, so the
    # source column (last real cell) is $(NF-1).
    name=$2; type=$3; def=$4; src=$(NF-1);
    gsub(/^[ \t]+|[ \t]+$/,"",name); gsub(/\*\*/,"",name);
    gsub(/^[ \t]+|[ \t]+$/,"",type);
    gsub(/^[ \t]+|[ \t]+$/,"",def);
    gsub(/^[ \t]+|[ \t]+$/,"",src);
    total++;
    t=tolower(type);
    if (t ~ /secret/)       { sec++;  bucket="SECRET" }
    else if (t ~ /flag/)    { ff++;   bucket="FLAG" }
    else if (t ~ /dial/)    { dial++; bucket="DIAL" }
    else                    { other++; bucket="OTHER" }
    # Emit one normalized row per knob, tagged with bucket + phase + source.
    printf("%s\t%s\t%s\t%s\t%s\n", bucket, phase, name, def, src);
  }
  END {
    printf("__COUNTS__\t%d\t%d\t%d\t%d\t%d\n", total, sec, ff, dial, other) > "/dev/stderr"
  }
' "$MANIFEST" 2> "$tmp.counts" > "$tmp.rows"

# The counts line was written to stderr as a single TSV line: __COUNTS__\tTOTAL\tSEC\tFF\tDIAL\tOTHER
IFS=$'\t' read -r _ TOTAL SEC FF DIAL OTHER < "$tmp.counts"

{
  echo "# Config reference (GENERATED — do not edit by hand)"
  echo
  echo "**Generated from** [\`config-manifest.md\`](config-manifest.md) **by** \`scripts/gen-config-reference.sh\`."
  echo "Regenerate on any manifest change; CI verifies this file is up to date"
  echo "(PROJECT_RULES.md §8 — docs staleness is a failing check)."
  echo
  echo "> This is the always-current index of **what is toggleable**. The manifest remains the"
  echo "> authoritative source (with blast-radius, mis-setting effects, and full rationale); this"
  echo "> reference is the derived, at-a-glance list. Type vocabulary: **secret** (custody matters),"
  echo "> **feature-flag** (on/off or mode/selector), **dial** (a tunable number/window/TTL)."
  echo
  echo "## Counts"
  echo
  echo "| bucket | count |"
  echo "|---|---|"
  echo "| secrets | ${SEC} |"
  echo "| feature-flags | ${FF} |"
  echo "| dials | ${DIAL} |"
  echo "| other/uncategorized | ${OTHER} |"
  echo "| **total knobs** | **${TOTAL}** |"
  echo
  echo "> One-way doors are **not** dials: the frozen contracts (response shape, LOG, hashing,"
  echo "> vocabulary, never-fork) live in code (\`control-plane/internal/{vocab,hash,verdict,store,router}\`),"
  echo "> not in this toggle list. A dial that *feeds* a door (e.g. a \`confidence\` number vs. the"
  echo "> \`confidence\` int-0–100 *type*) is a dial; the type it feeds is frozen."
  echo

  for bucket in SECRET FLAG DIAL OTHER; do
    case "$bucket" in
      SECRET) title="Secrets" ;;
      FLAG)   title="Feature-flags" ;;
      DIAL)   title="Dials" ;;
      OTHER)  title="Other / uncategorized" ;;
    esac
    n=$(awk -F'\t' -v b="$bucket" '$1==b' "$tmp.rows" | wc -l | tr -d ' ')
    [ "$n" -eq 0 ] && continue
    echo "## ${title} (${n})"
    echo
    echo "| knob | default / state | source |"
    echo "|---|---|---|"
    awk -F'\t' -v b="$bucket" '$1==b { printf("| %s | %s | %s |\n", $3, $4, $5) }' "$tmp.rows"
    echo
  done

  echo "---"
  echo
  echo "*End generated reference. Source of truth: [\`config-manifest.md\`](config-manifest.md).*"
} > "$OUT"

echo "gen-config-reference: wrote $OUT (${TOTAL} knobs: ${SEC} secrets, ${FF} flags, ${DIAL} dials, ${OTHER} other)"
