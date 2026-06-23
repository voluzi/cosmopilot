#!/usr/bin/env bash
#
# Generates markdown documentation files from example YAML files.
# Each YAML file in examples/<chain>/ becomes a markdown file in docs/docs/examples/<chain>/
#

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"

EXAMPLES_DIR="${ROOT_DIR}/examples"
DOCS_EXAMPLES_DIR="${ROOT_DIR}/docs/docs/examples"

# Convert filename to title (e.g., "mainnet-fullnode" -> "Mainnet Fullnode")
filename_to_title() {
    local filename="$1"
    echo "$filename" | sed 's/\.yaml$//' | sed 's/[-_]/ /g' | awk '{for(i=1;i<=NF;i++) $i=toupper(substr($i,1,1)) tolower(substr($i,2))}1'
}

# Capitalize chain name (e.g., "nibiru" -> "Nibiru", "cosmoshub" -> "Cosmoshub")
capitalize_chain() {
    local chain="$1"
    echo "$chain" | awk '{print toupper(substr($0,1,1)) tolower(substr($0,2))}'
}

# Clean up existing docs examples
# Remove previously generated markdown files, but keep the curated `_category_.json`
# files (they hold the sidebar labels and ordering) so regeneration is idempotent.
if [ -d "${DOCS_EXAMPLES_DIR}" ]; then
    find "${DOCS_EXAMPLES_DIR}" -mindepth 2 -type f -name '*.md' -delete
fi

# Process each chain directory
for chain_dir in "${EXAMPLES_DIR}"/*/; do
    [ -d "$chain_dir" ] || continue

    chain_name=$(basename "$chain_dir")
    chain_title=$(capitalize_chain "$chain_name")
    docs_chain_dir="${DOCS_EXAMPLES_DIR}/${chain_name}"

    mkdir -p "$docs_chain_dir"

    # Create a default category file for chains that don't have a curated one yet.
    category_file="${docs_chain_dir}/_category_.json"
    if [ ! -f "$category_file" ]; then
        # Escape backslashes and double quotes so the label is always valid JSON.
        json_label="${chain_title//\\/\\\\}"
        json_label="${json_label//\"/\\\"}"
        printf '{\n  "label": "%s"\n}\n' "$json_label" > "$category_file"
    fi

    # Process each YAML file in the chain directory
    for yaml_file in "${chain_dir}"*.yaml; do
        [ -f "$yaml_file" ] || continue

        filename=$(basename "$yaml_file")
        md_filename="${filename%.yaml}.md"

        file_title=$(filename_to_title "$filename")
        full_title="${chain_title} ${file_title}"

        # Generate markdown file
        {
            echo "# ${full_title}"
            echo ""
            echo '```yaml'
            cat "$yaml_file"
            echo ""
            echo '```'
        } > "${docs_chain_dir}/${md_filename}"

        echo "Generated: docs/docs/examples/${chain_name}/${md_filename}"
    done
done

echo "Done generating example docs."
