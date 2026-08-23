#!/usr/bin/env bash
set -Eeuo pipefail

export LC_ALL=C
umask 077

script_dir=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
project_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
policy_file="$project_dir/policy/routes.json"
output_dir="$project_dir/dist/ruleset"
version_file="$project_dir/.sing-box-version"
converter="$script_dir/convert-rule-list.sh"
singbox_request="${SINGBOX_BIN:-sing-box}"

fail() { printf 'error: %s\n' "$*" >&2; exit 1; }
usage() { printf '%s\n' 'usage: build-rules.sh [--policy FILE]'; }

while (( $# > 0 )); do
  case "$1" in
    --policy) (( $# >= 2 )) || fail "--policy requires a file"; policy_file=$2; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done

command -v jq >/dev/null 2>&1 || fail "jq is required"
[[ -f "$policy_file" ]] || fail "missing route policy: $policy_file"
[[ -f "$version_file" ]] || fail "missing version file: $version_file"
[[ -x "$converter" ]] || fail "missing executable rule converter: $converter"

if [[ "$singbox_request" == */* ]]; then
  [[ -x "$singbox_request" ]] || fail "sing-box is not executable: $singbox_request"
  singbox_bin=$singbox_request
else
  singbox_bin=$(command -v "$singbox_request") || fail "sing-box not found: $singbox_request"
fi
expected_version=$(tr -d '[:space:]' < "$version_file")
[[ "$expected_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.-]+)?$ ]] || fail "invalid version in $version_file"
actual_version=$("$singbox_bin" version | sed -n '1s/^sing-box version[[:space:]]*//p')
[[ -n "$actual_version" ]] || fail "unable to read sing-box version"
[[ "$actual_version" == "$expected_version" ]] || fail "sing-box version mismatch: expected $expected_version, got $actual_version"

output_parent=$(dirname -- "$output_dir")
output_name=$(basename -- "$output_dir")
[[ -n "$output_name" && "$output_name" != "." && "$output_name" != "/" ]] || fail "unsafe output directory: $output_dir"
mkdir -p -- "$output_parent"
stage_dir=$(mktemp -d "$output_parent/.${output_name}.build.XXXXXX")
route_list=$(mktemp "$output_parent/.routes.XXXXXX")
previous_dir="$output_parent/.${output_name}.previous.$$"

cleanup() {
  [[ -z "${route_list:-}" || ! -e "$route_list" ]] || rm -f -- "$route_list"
  [[ -z "${stage_dir:-}" || ! -d "$stage_dir" ]] || rm -rf -- "$stage_dir"
  if [[ -d "$previous_dir" && ! -e "$output_dir" ]]; then mv -- "$previous_dir" "$output_dir"; fi
}
trap cleanup EXIT INT TERM

jq -er '
  def valid_tag: type == "string" and test("^[A-Za-z0-9][A-Za-z0-9._-]*$");
  def valid_source: type == "string" and test("^rules/[A-Za-z0-9][A-Za-z0-9._-]*\\.txt$");
  if .version != 1 then error("policy/routes.json: unsupported version") else . end
  | if (.routes | type) != "array" or (.routes | length) == 0 then error("policy/routes.json: routes must be a non-empty array") else . end
  | .routes as $routes
  | if any($routes[]; (.tag | valid_tag) | not) then error("policy/routes.json: invalid route tag") else . end
  | if any($routes[]; (.source | valid_source) | not) then error("policy/routes.json: source must be a safe rules/*.txt path") else . end
  | if any($routes[]; (.priority | type) != "number" or (.priority | floor) != .priority or .priority < 0) then error("policy/routes.json: priority must be a non-negative integer") else . end
  | if ([$routes[].tag] | length) != ([$routes[].tag] | unique | length) then error("policy/routes.json: duplicate route tag") else . end
  | if ([$routes[].source] | length) != ([$routes[].source] | unique | length) then error("policy/routes.json: duplicate route source") else . end
  | if ([$routes[].priority] | length) != ([$routes[].priority] | unique | length) then error("policy/routes.json: duplicate route priority") else . end
  | if any($routes[]; (.source | capture("^rules/(?<stem>.+)\\.txt$").stem) != .tag) then error("policy/routes.json: tag must match the source filename") else . end
  | $routes | sort_by(.priority) | .[] | [.tag, .source] | @tsv
' "$policy_file" > "$route_list"

shopt -s nullglob
rule_sources=("$project_dir"/rules/*.txt)
legacy_rule_sources=("$project_dir"/rules/*.json)
shopt -u nullglob
(( ${#rule_sources[@]} > 0 )) || fail "no rule source found in $project_dir/rules"
(( ${#legacy_rule_sources[@]} == 0 )) || fail "legacy rules/*.json sources are not allowed; migrate them to .txt"
for source_path in "${rule_sources[@]}"; do
  relative_source="rules/$(basename -- "$source_path")"
  jq -e --arg source "$relative_source" 'any(.routes[]; .source == $source)' "$policy_file" >/dev/null \
    || fail "orphan rule source is not declared by policy/routes.json: $relative_source"
done

rule_count=0
while IFS="$(printf '\t')" read -r tag source; do
  [[ -n "$tag" && -n "$source" ]] || fail "invalid empty route entry"
  source_path="$project_dir/$source"
  [[ -f "$source_path" && ! -L "$source_path" ]] || fail "missing or unsafe rule source: $source"
  source_json="$stage_dir/.$tag.source.json"
  printf 'convert %s -> temporary JSON\n' "$source"
  "$converter" "$source_path" --output "$source_json"
  printf 'compile temporary JSON -> %s.srs\n' "$tag"
  "$singbox_bin" rule-set compile "$source_json" -o "$stage_dir/$tag.srs"
  rm -f -- "$source_json"
  rule_count=$((rule_count + 1))
done < "$route_list"
(( rule_count > 0 )) || fail "no rule set declared by $policy_file"

if command -v sha256sum >/dev/null 2>&1; then
  hash_file() { sha256sum "$1" | awk -v name="$2" '{ print $1 "  " name }'; }
elif command -v shasum >/dev/null 2>&1; then
  hash_file() { shasum -a 256 "$1" | awk -v name="$2" '{ print $1 "  " name }'; }
else
  fail "neither sha256sum nor shasum is available"
fi
while IFS= read -r tag; do
  hash_file "$stage_dir/$tag.srs" "$tag.srs"
done < <(jq -r '.routes | sort_by(.priority)[] | .tag' "$policy_file") > "$stage_dir/SHA256SUMS"

[[ ! -L "$output_dir" ]] || fail "refusing to replace symlink: $output_dir"
[[ ! -e "$previous_dir" ]] || fail "temporary previous directory already exists: $previous_dir"
if [[ -d "$output_dir" ]]; then
  mv -- "$output_dir" "$previous_dir"
elif [[ -e "$output_dir" ]]; then
  fail "output path exists and is not a directory: $output_dir"
fi
mv -- "$stage_dir" "$output_dir"
stage_dir=""
rm -f -- "$route_list"
route_list=""
[[ ! -d "$previous_dir" ]] || rm -rf -- "$previous_dir"
trap - EXIT INT TERM
printf 'built %s rule sets in %s\n' "$rule_count" "$output_dir"
