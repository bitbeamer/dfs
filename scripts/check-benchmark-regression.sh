#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 BASELINE.txt CANDIDATE.txt" >&2
  exit 2
fi

awk '
function key(name) {
  sub(/-[0-9]+$/, "", name)
  return name
}
function limit(name) {
  if (name ~ /^BenchmarkBaselineCachedContent\//) return 1.05
  if (name ~ /^BenchmarkBaselineNamespace\//) return 1.10
  if (name ~ /^BenchmarkBaselineMutations\//) return 1.15
  return 0
}
function collect(file_number, name, ns, bytes) {
  name = key(name)
  if (limit(name) == 0) return
  if (file_number == 1) {
    base_ns[name] += ns; base_bytes[name] += bytes; base_count[name]++
  } else {
    candidate_ns[name] += ns; candidate_bytes[name] += bytes; candidate_count[name]++
  }
}
FNR == 1 { file_number++ }
$1 ~ /^BenchmarkBaseline/ {
  ns = bytes = -1
  for (column = 2; column <= NF; column++) {
    if ($(column + 1) == "ns/op") ns = $column
    if ($(column + 1) == "B/op") bytes = $column
  }
  if (ns >= 0 && bytes >= 0) collect(file_number, $1, ns, bytes)
}
END {
  failed = 0
  for (name in base_count) {
    if (!candidate_count[name]) {
      printf "FAIL %-58s missing from candidate\n", name
      failed = 1
      continue
    }
    old_ns = base_ns[name] / base_count[name]
    new_ns = candidate_ns[name] / candidate_count[name]
    old_bytes = base_bytes[name] / base_count[name]
    new_bytes = candidate_bytes[name] / candidate_count[name]
    time_limit = limit(name)
    time_ok = new_ns <= old_ns * time_limit
    bytes_ok = new_bytes <= old_bytes * 1.10
    printf "%s %-58s time=%0.2fx (limit %0.2fx) bytes=%0.2fx (limit 1.10x)\n", \
      (time_ok && bytes_ok ? "PASS" : "FAIL"), name, new_ns / old_ns, time_limit, \
      (old_bytes == 0 ? (new_bytes == 0 ? 1 : 999) : new_bytes / old_bytes)
    if (!time_ok || !bytes_ok) failed = 1
  }
  for (name in candidate_count) {
    if (!base_count[name]) {
      printf "FAIL %-58s missing from baseline\n", name
      failed = 1
    }
  }
  exit failed
}
' "$1" "$2"
