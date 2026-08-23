#!/usr/bin/env bash
set -Eeuo pipefail

export LC_ALL=C
umask 077

script_dir=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
project_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
env_file="$project_dir/.env"
routes_file="$project_dir/policy/routes.json"
tests_file="$project_dir/tests/routing-cases.json"
converter_tests_file="$project_dir/tests/rule-list-syntax.txt"
version_file="$project_dir/.sing-box-version"
singbox_request="${SINGBOX_BIN:-sing-box}"

fail() { printf 'error: %s\n' "$*" >&2; exit 1; }
usage() { printf '%s\n' 'usage: validate.sh [--env-file FILE] [--tests FILE]'; }

while (( $# > 0 )); do
  case "$1" in
    --env-file) (( $# >= 2 )) || fail "--env-file requires a file"; env_file=$2; shift 2 ;;
    --tests) (( $# >= 2 )) || fail "--tests requires a file"; tests_file=$2; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done

command -v jq >/dev/null 2>&1 || fail "jq is required"
for required_file in "$env_file" "$routes_file" "$tests_file" "$converter_tests_file" "$version_file"; do
  [[ -f "$required_file" ]] || fail "missing required file: $required_file"
done

if [[ "$singbox_request" == */* ]]; then
  [[ -x "$singbox_request" ]] || fail "sing-box is not executable: $singbox_request"
  singbox_bin=$singbox_request
else
  singbox_bin=$(command -v "$singbox_request") || fail "sing-box not found: $singbox_request"
fi
expected_version=$(tr -d '[:space:]' < "$version_file")
actual_version=$("$singbox_bin" version | sed -n '1s/^sing-box version[[:space:]]*//p')
[[ -n "$actual_version" ]] || fail "unable to read sing-box version"
[[ "$actual_version" == "$expected_version" ]] || fail "sing-box version mismatch: expected $expected_version, got $actual_version"
export SINGBOX_BIN="$singbox_bin"

jq -e '
  if .version != 1 then error("tests/routing-cases.json: unsupported version") else . end
  | if (.cases | type) != "array" or (.cases | length) == 0 then error("tests/routing-cases.json: cases must be a non-empty array") else . end
  | if any(.cases[];
      (((has("domain") and (has("ip") | not)) or (has("ip") and (has("domain") | not))) | not)
      or ((has("domain")) and ((.domain | type) != "string" or (.domain | test("^[A-Za-z0-9._-]+$") | not)))
      or ((has("ip")) and ((.ip | type) != "string" or (.ip | test("^[0-9A-Fa-f:.]+$") | not)))
      or ((.ruleset != null) and ((.ruleset | type) != "string"))
      or (.outbound | type) != "string"
      or ((has("domain")) and ((.dns | type) != "string"))
      or ((has("ip")) and has("dns")))
    then error("tests/routing-cases.json: invalid case") else . end
  | if ([.cases[] | (.domain // .ip)] | length) != ([.cases[] | (.domain // .ip)] | unique | length)
    then error("tests/routing-cases.json: duplicate domain or IP") else . end
  | ([.cases[].ruleset | select(. != null)] | unique) as $tested_rulesets
  | ([$routes[0].routes[].tag] | unique) as $route_tags
  | if (($tested_rulesets - $route_tags) | length) > 0
    then error("tests/routing-cases.json: case references an unknown ruleset") else . end
  | if (($route_tags - $tested_rulesets) | length) > 0
    then error("tests/routing-cases.json: every route must have a routing case") else . end
' --slurpfile routes "$routes_file" "$tests_file" >/dev/null

mkdir -p -- "$project_dir/dist"
stage_dir=$(mktemp -d "$project_dir/dist/.validate.XXXXXX")
route_list=$(mktemp "$project_dir/dist/.validate-routes.XXXXXX")
case_list=$(mktemp "$project_dir/dist/.validate-cases.XXXXXX")

cleanup() {
  [[ -z "${stage_dir:-}" || ! -d "$stage_dir" ]] || rm -rf -- "$stage_dir"
  [[ -z "${route_list:-}" || ! -e "$route_list" ]] || rm -f -- "$route_list"
  [[ -z "${case_list:-}" || ! -e "$case_list" ]] || rm -f -- "$case_list"
}
trap cleanup EXIT INT TERM

converter_json="$stage_dir/converter.json"
converter_log="$stage_dir/converter.log"
"$script_dir/convert-rule-list.sh" "$converter_tests_file" --output "$converter_json" 2>"$converter_log"
jq -e '
  .version == 2
  and (.rules | length) == 2
  and .rules[0].domain == ["exact.example"]
  and .rules[0].domain_suffix == ["suffix.example"]
  and .rules[0].domain_keyword == ["keyword-example"]
  and .rules[0].domain_regex == ["^edge-[a-z]{1,3},region\\.example$"]
  and .rules[0].ip_cidr == ["192.0.2.0/24", "2001:db8::/32"]
  and .rules[1].process_name == ["com.example.app"]
' "$converter_json" >/dev/null
grep -F 'IP-ASN' "$converter_log" >/dev/null
grep -F '64500' "$converter_log" >/dev/null
"$singbox_bin" rule-set compile "$converter_json" -o "$stage_dir/converter.srs"
for converter_match in \
  exact.example \
  sub.suffix.example \
  api-keyword-example.test \
  'edge-abc,region.example' \
  192.0.2.1 \
  2001:db8::1; do
  converter_match_output=$("$singbox_bin" rule-set match -f binary "$stage_dir/converter.srs" "$converter_match" 2>&1)
  [[ "$converter_match_output" == match\ * ]] \
    || fail "converted rule set did not match: $converter_match"
done
converter_match_output=$("$singbox_bin" rule-set match -f binary "$stage_dir/converter.srs" unmatched.example 2>&1)
[[ -z "$converter_match_output" ]] || fail "converted rule set unexpectedly matched: unmatched.example"

printf '\357\273\277# BOM and CRLF\r\nDOMAIN,bom.example\r\nIP-CIDR,198.51.100.0/24,no-resolve' > "$stage_dir/compat.txt"
"$script_dir/convert-rule-list.sh" "$stage_dir/compat.txt" --output "$stage_dir/compat.json" >/dev/null
jq -e '
  .rules[0].domain == ["bom.example"]
  and .rules[0].ip_cidr == ["198.51.100.0/24"]
' "$stage_dir/compat.json" >/dev/null

printf '%s\n' 'UNKNOWN,invalid.example' > "$stage_dir/invalid.txt"
if "$script_dir/convert-rule-list.sh" "$stage_dir/invalid.txt" --output "$stage_dir/invalid.json" >/dev/null 2>&1; then
  fail "rule converter accepted an unknown rule type"
fi
printf '%s\n' 'IP-CIDR,192.0.2.0/24,resolve' > "$stage_dir/invalid-flag.txt"
if "$script_dir/convert-rule-list.sh" "$stage_dir/invalid-flag.txt" --output "$stage_dir/invalid-flag.json" >/dev/null 2>&1; then
  fail "rule converter accepted an unknown CIDR flag"
fi
printf '%s\n' 'DOMAIN,invalid.example,no-resolve' > "$stage_dir/invalid-domain.txt"
if "$script_dir/convert-rule-list.sh" "$stage_dir/invalid-domain.txt" --output "$stage_dir/invalid-domain.json" >/dev/null 2>&1; then
  fail "rule converter accepted an extra domain parameter"
fi
printf '%s\n' 'IP-ASN,64500,no-resolve' > "$stage_dir/asn-only.txt"
if "$script_dir/convert-rule-list.sh" "$stage_dir/asn-only.txt" --output "$stage_dir/asn-only.json" >/dev/null 2>&1; then
  fail "rule converter accepted a source containing only ignored IP-ASN rules"
fi
printf '%s\n' 'DOMAIN,valid.example' 'IP-ASN,64500,unexpected-flag' > "$stage_dir/invalid-asn-flag.txt"
if "$script_dir/convert-rule-list.sh" "$stage_dir/invalid-asn-flag.txt" --output "$stage_dir/invalid-asn-flag.json" >/dev/null 2>&1; then
  fail "rule converter accepted an unknown IP-ASN flag"
fi
printf '%s\n' 'PROCESS-NAME,valid-app,unexpected-parameter' > "$stage_dir/invalid-process-name.txt"
if "$script_dir/convert-rule-list.sh" "$stage_dir/invalid-process-name.txt" --output "$stage_dir/invalid-process-name.json" >/dev/null 2>&1; then
  fail "rule converter accepted an extra PROCESS-NAME parameter"
fi
rm -f -- "$converter_json" "$stage_dir/converter.srs" "$stage_dir/compat.json"

"$script_dir/build-rules.sh"
mkdir -p -- "$stage_dir/ruleset"
cp -p -- "$project_dir"/dist/ruleset/*.srs "$stage_dir/ruleset/"
cp -p -- "$project_dir/dist/ruleset/SHA256SUMS" "$stage_dir/ruleset/"
"$script_dir/render-config.sh" \
  --env-file "$env_file" \
  --output "$stage_dir/config.json" \
  --ruleset-dir "$stage_dir/ruleset"

"$singbox_bin" check -C "$stage_dir"

jq -e '
  (.dns.servers | length) == 1
  and .dns.servers[0].tag == "dns-direct"
  and .dns.servers[0].type == "https"
  and .dns.servers[0].server == "1.1.1.1"
  and (.dns.servers[0] | has("detour") | not)
  and .dns.final == "dns-direct"
  and (.dns.rules | length) == 0
  and .route.default_domain_resolver == "dns-direct"
' "$stage_dir/config.json" >/dev/null

jq -er '
  .routes
  | sort_by(.priority)[]
  | [.tag, .outbound]
  | @tsv
' "$routes_file" > "$route_list"
default_dns=$(jq -er '.dns.final' "$stage_dir/config.json")
jq -er '
  .cases[]
  | [
      (.domain // .ip),
      (if has("domain") then "domain" else "ip" end),
      (.ruleset // "__none__"),
      .outbound,
      (.dns // "__not_applicable__")
    ]
  | @tsv
' "$tests_file" > "$case_list"

case_count=0
while IFS="$(printf '\t')" read -r test_input case_kind expected_ruleset expected_outbound expected_dns; do
  [[ -n "$test_input" && -n "$case_kind" && -n "$expected_outbound" && -n "$expected_dns" ]] || fail "invalid empty routing test case"
  [[ "$expected_ruleset" != "__none__" ]] || expected_ruleset=""
  actual_ruleset=""
  actual_outbound="direct"
  actual_dns=$default_dns

  while IFS="$(printf '\t')" read -r route_tag route_outbound; do
    match_output=$("$singbox_bin" rule-set match -f binary "$stage_dir/ruleset/$route_tag.srs" "$test_input" 2>&1)
    if [[ "$match_output" == match\ * ]]; then
      actual_ruleset=$route_tag
      actual_outbound=$route_outbound
      break
    fi
  done < "$route_list"

  [[ "$actual_ruleset" == "$expected_ruleset" ]] \
    || fail "routing case $test_input: expected ruleset ${expected_ruleset:-<none>}, got ${actual_ruleset:-<none>}"
  [[ "$actual_outbound" == "$expected_outbound" ]] \
    || fail "routing case $test_input: expected outbound $expected_outbound, got $actual_outbound"
  if [[ "$case_kind" == "ip" ]]; then
    printf 'case %s -> %s (literal IP; DNS not applicable)\n' "$test_input" "$actual_outbound"
  elif [[ "$case_kind" == "domain" ]]; then
    [[ "$actual_dns" == "$expected_dns" ]] \
      || fail "routing case $test_input: expected DNS $expected_dns, got $actual_dns"
    printf 'case %s -> %s / %s\n' "$test_input" "$actual_outbound" "$actual_dns"
  else
    fail "routing case $test_input: unknown case kind $case_kind"
  fi
  case_count=$((case_count + 1))
done < "$case_list"

(( case_count > 0 )) || fail "no routing test case executed"
trap - EXIT INT TERM
cleanup
printf 'validated config and %s routing cases with sing-box %s\n' "$case_count" "$actual_version"
