#!/usr/bin/env bash
set -Eeuo pipefail

export LC_ALL=C
umask 077

fail() { printf 'error: %s\n' "$*" >&2; exit 1; }
usage() { printf '%s\n' 'usage: convert-rule-list.sh SOURCE [--output FILE]'; }

(( $# > 0 )) || { usage >&2; exit 1; }
case "$1" in
  -h|--help) usage; exit 0 ;;
esac

source_file=$1
shift
output_file=""

while (( $# > 0 )); do
  case "$1" in
    --output)
      (( $# >= 2 )) || fail "--output requires a file"
      [[ -z "$output_file" ]] || fail "--output may only be specified once"
      output_file=$2
      shift 2
      ;;
    -h|--help) usage; exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done

command -v jq >/dev/null 2>&1 || fail "jq is required"
[[ -f "$source_file" ]] || fail "source is not a regular file: $source_file"

if [[ -z "$output_file" ]]; then
  source_dir=$(dirname -- "$source_file")
  source_name=$(basename -- "$source_file")
  case "$source_name" in
    .*)
      hidden_tail=${source_name#.}
      if [[ "$hidden_tail" == *.* ]]; then
        output_name=${source_name%.*}.json
      else
        output_name=$source_name.json
      fi
      ;;
    *.*) output_name=${source_name%.*}.json ;;
    *) output_name=$source_name.json ;;
  esac
  output_file=$source_dir/$output_name
fi

[[ -n "$output_file" && "$output_file" != */ ]] || fail "unsafe output file: $output_file"
output_parent=$(dirname -- "$output_file")
output_name=$(basename -- "$output_file")
[[ -n "$output_name" && "$output_name" != "." && "$output_name" != "/" ]] \
  || fail "unsafe output file: $output_file"
[[ ! -L "$output_file" ]] || fail "refusing to replace symlink: $output_file"
[[ ! -e "$output_file" || -f "$output_file" ]] \
  || fail "output exists and is not a regular file: $output_file"
[[ ! -e "$output_file" || ! "$source_file" -ef "$output_file" ]] \
  || fail "source and output must be different files: $source_file"

mkdir -p -- "$output_parent"
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/convert-rule-list.XXXXXX")
stage_file=""

cleanup() {
  [[ -z "${stage_file:-}" || ! -e "$stage_file" ]] || rm -f -- "$stage_file"
  [[ -z "${work_dir:-}" || ! -d "$work_dir" ]] || rm -rf -- "$work_dir"
}
trap cleanup EXIT INT TERM

for field in domain domain_suffix domain_keyword domain_regex ip_cidr process_name; do
  : > "$work_dir/$field"
done

trim() {
  trimmed=$1
  trimmed=${trimmed#"${trimmed%%[![:space:]]*}"}
  trimmed=${trimmed%"${trimmed##*[![:space:]]}"}
}

append_rule() {
  printf '%s\n' "$2" >> "$work_dir/$1"
  accepted_count=$((accepted_count + 1))
}

line_number=0
accepted_count=0
asn_count=0
while IFS= read -r line || [[ -n "$line" ]]; do
  line_number=$((line_number + 1))
  if (( line_number == 1 )); then
    line=${line#$'\xEF\xBB\xBF'}
  fi
  [[ "$line" != *$'\r' ]] || line=${line%$'\r'}
  trim "$line"
  line=$trimmed
  [[ -n "$line" && "$line" != \#* ]] || continue

  [[ "$line" == *,* ]] || fail "$source_file:$line_number: rule must contain a comma"
  rule_type=${line%%,*}
  rule_rest=${line#*,}
  trim "$rule_type"
  rule_type=$trimmed
  trim "$rule_rest"
  rule_rest=$trimmed
  [[ -n "$rule_type" ]] || fail "$source_file:$line_number: rule type is empty"
  [[ -n "$rule_rest" ]] || fail "$source_file:$line_number: $rule_type value is empty"

  case "$rule_type" in
    DOMAIN|DOMAIN-SUFFIX|DOMAIN-KEYWORD)
      [[ "$rule_rest" != *,* ]] \
        || fail "$source_file:$line_number: $rule_type does not accept extra parameters"
      [[ "$rule_rest" != *[[:space:]]* ]] \
        || fail "$source_file:$line_number: $rule_type value must not contain whitespace"
      case "$rule_type" in
        DOMAIN) field=domain ;;
        DOMAIN-SUFFIX) field=domain_suffix ;;
        DOMAIN-KEYWORD) field=domain_keyword ;;
      esac
      append_rule "$field" "$rule_rest"
      ;;
    DOMAIN-REGEX)
      append_rule domain_regex "$rule_rest"
      ;;
    PROCESS-NAME)
      [[ "$rule_rest" != *,* ]] \
        || fail "$source_file:$line_number: PROCESS-NAME does not accept extra parameters"
      append_rule process_name "$rule_rest"
      ;;
    IP-CIDR|IP-CIDR6)
      cidr=$rule_rest
      cidr_flag=""
      if [[ "$rule_rest" == *,* ]]; then
        cidr=${rule_rest%%,*}
        cidr_flag=${rule_rest#*,}
        trim "$cidr"
        cidr=$trimmed
        trim "$cidr_flag"
        cidr_flag=$trimmed
        [[ "$cidr_flag" == "no-resolve" ]] \
          || fail "$source_file:$line_number: unknown $rule_type flag: ${cidr_flag:-<empty>}"
      fi
      [[ -n "$cidr" ]] || fail "$source_file:$line_number: $rule_type value is empty"
      append_rule ip_cidr "$cidr"
      ;;
    IP-ASN)
      asn_value=$rule_rest
      asn_flag=""
      if [[ "$rule_rest" == *,* ]]; then
        asn_value=${rule_rest%%,*}
        asn_flag=${rule_rest#*,}
        trim "$asn_flag"
        asn_flag=$trimmed
        [[ "$asn_flag" == "no-resolve" ]] \
          || fail "$source_file:$line_number: unknown IP-ASN flag: ${asn_flag:-<empty>}"
      fi
      trim "$asn_value"
      asn_value=$trimmed
      [[ -n "$asn_value" ]] || fail "$source_file:$line_number: IP-ASN value is empty"
      [[ "$asn_value" =~ ^[0-9]+$ ]] \
        || fail "$source_file:$line_number: IP-ASN value must be a decimal number"
      printf 'warning: %s:%s: skipped IP-ASN value %s\n' \
        "$source_file" "$line_number" "$asn_value" >&2
      asn_count=$((asn_count + 1))
      ;;
    *) fail "$source_file:$line_number: unknown rule type: $rule_type" ;;
  esac
done < "$source_file"

(( accepted_count > 0 )) \
  || fail "$source_file: no convertible rules found (IP-ASN rules are ignored)"

stage_file=$(mktemp "$output_parent/.${output_name}.tmp.XXXXXX")
chmod 0600 "$stage_file"
jq -n \
  --rawfile domain "$work_dir/domain" \
  --rawfile domain_suffix "$work_dir/domain_suffix" \
  --rawfile domain_keyword "$work_dir/domain_keyword" \
  --rawfile domain_regex "$work_dir/domain_regex" \
  --rawfile ip_cidr "$work_dir/ip_cidr" \
  --rawfile process_name "$work_dir/process_name" \
  '
    def values($input):
      $input | split("\n") | map(select(length > 0)) | unique;
    {
      version: 2,
      rules: [
        ([
          {key: "domain", value: values($domain)},
          {key: "domain_suffix", value: values($domain_suffix)},
          {key: "domain_keyword", value: values($domain_keyword)},
          {key: "domain_regex", value: values($domain_regex)},
          {key: "ip_cidr", value: values($ip_cidr)}
        ] | map(select(.value | length > 0)) | from_entries),
        (if (values($process_name) | length) > 0
          then {process_name: values($process_name)}
          else {}
        end)
      ] | map(select(length > 0))
    }
  ' > "$stage_file"

unique_count=$(jq -er '[.rules[] | to_entries[] | .value | length] | add // 0' "$stage_file")
duplicate_count=$((accepted_count - unique_count))
[[ ! -L "$output_file" ]] || fail "refusing to replace symlink: $output_file"
[[ ! -e "$output_file" || -f "$output_file" ]] \
  || fail "output exists and is not a regular file: $output_file"
mv -f -- "$stage_file" "$output_file"
stage_file=""
trap - EXIT INT TERM
rm -rf -- "$work_dir"
work_dir=""
printf 'converted %s unique rules to %s (%s duplicates removed, %s IP-ASN skipped)\n' \
  "$unique_count" "$output_file" "$duplicate_count" "$asn_count"
