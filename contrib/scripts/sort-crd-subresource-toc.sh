#!/usr/bin/env bash

set -euo pipefail

file="$1"
tmp_file="$(mktemp)"
trap 'rm -f "${tmp_file}"' EXIT

awk '
BEGIN {
    command = "LC_ALL=C sort | cut -f2-"
}
$0 == "### Sub Resources" {
    print
    collecting = 1
    next
}
collecting && /^\* / {
    label = $0
    sub(/^\* \[/, "", label)
    sub(/\].*$/, "", label)
    print label "\t" $0 | command
    seen = 1
    next
}
collecting && seen {
    close(command)
    collecting = 0
}
{
    print
}
' "${file}" > "${tmp_file}"

mv "${tmp_file}" "${file}"
trap - EXIT
