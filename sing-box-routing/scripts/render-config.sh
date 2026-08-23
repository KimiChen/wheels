#!/usr/bin/env bash
set -Eeuo pipefail

export LC_ALL=C
umask 077

script_dir=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
project_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
base_file="$project_dir/config/gateway.base.json"
nodes_file="$project_dir/inventory/nodes.json"
groups_file="$project_dir/policy/egress-groups.json"
routes_file="$project_dir/policy/routes.json"
env_file="$project_dir/.env"
output_file="$project_dir/dist/config.json"
ruleset_dir=""

fail() { printf 'error: %s\n' "$*" >&2; exit 1; }
require_safe_output_target() {
  if [[ -e "$output_file" || -L "$output_file" ]]; then
    [[ -f "$output_file" && ! -L "$output_file" ]] \
      || fail "output path exists and is not a regular file: $output_file"
  fi
}
usage() {
  printf '%s\n' 'usage: render-config.sh [--env-file FILE] [--output FILE] [--ruleset-dir DIR]'
  printf '%s\n' '                        [--base FILE] [--nodes FILE] [--groups FILE] [--routes FILE]'
  printf '%s\n' 'default ruleset directory: TARGET_CONFIG_DIR/ruleset from the dotenv file'
}

while (( $# > 0 )); do
  case "$1" in
    --env-file) (( $# >= 2 )) || fail "--env-file requires a file"; env_file=$2; shift 2 ;;
    --output) (( $# >= 2 )) || fail "--output requires a file"; output_file=$2; shift 2 ;;
    --ruleset-dir) (( $# >= 2 )) || fail "--ruleset-dir requires a directory"; ruleset_dir=$2; shift 2 ;;
    --base) (( $# >= 2 )) || fail "--base requires a file"; base_file=$2; shift 2 ;;
    --nodes) (( $# >= 2 )) || fail "--nodes requires a file"; nodes_file=$2; shift 2 ;;
    --groups) (( $# >= 2 )) || fail "--groups requires a file"; groups_file=$2; shift 2 ;;
    --routes) (( $# >= 2 )) || fail "--routes requires a file"; routes_file=$2; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done

command -v jq >/dev/null 2>&1 || fail "jq is required"
for required_file in "$base_file" "$nodes_file" "$groups_file" "$routes_file" "$env_file"; do
  [[ -f "$required_file" ]] || fail "missing input file: $required_file"
done
[[ "$ruleset_dir" != "/" && "$ruleset_dir" != *$'\n'* ]] || fail "unsafe ruleset directory"

output_parent=$(dirname -- "$output_file")
output_name=$(basename -- "$output_file")
[[ -n "$output_name" && "$output_name" != "." && "$output_name" != "/" ]] || fail "unsafe output file: $output_file"
require_safe_output_target
mkdir -p -- "$output_parent"
temp_file=$(mktemp "$output_parent/.${output_name}.render.XXXXXX")
cleanup() { [[ -z "${temp_file:-}" || ! -e "$temp_file" ]] || rm -f -- "$temp_file"; }
trap cleanup EXIT INT TERM

# The dotenv file is data, never shell code. Process environment values override it.
jq -n -e \
  --slurpfile base "$base_file" \
  --slurpfile inventory "$nodes_file" \
  --slurpfile group_policy "$groups_file" \
  --slurpfile route_policy "$routes_file" \
  --rawfile dotenv "$env_file" \
  --arg ruleset_dir "${ruleset_dir%/}" '
  def bad($message): error($message);
  def valid_tag: type == "string" and test("^[A-Za-z0-9][A-Za-z0-9._-]*$");
  def valid_env_name: type == "string" and test("^[A-Za-z_][A-Za-z0-9_]*$");
  def unique_values: length == (unique | length);
  def trim: sub("^[ \\t]+"; "") | sub("[ \\t]+$"; "");
  def dotenv_value($raw; $line_number):
    ($raw | trim) as $value
    | if ($value | startswith("\"")) then
        if ($value | endswith("\"") | not) then bad(".env: unterminated double quote at line " + ($line_number | tostring))
        else try ($value | fromjson) catch bad(".env: invalid double-quoted value at line " + ($line_number | tostring)) end
      elif ($value | startswith("\u0027")) then
        if ($value | endswith("\u0027") | not) then bad(".env: unterminated single quote at line " + ($line_number | tostring))
        else $value[1:-1]
        end
      else $value
      end;
  def parse_dotenv:
    reduce (($dotenv | split("\n")) | to_entries[]) as $entry ({};
      ($entry.value | sub("\r$"; "")) as $line
      | if ($line | test("^[[:space:]]*(#|$)")) then .
        else
          (try ($line | capture("^[[:space:]]*(?:export[[:space:]]+)?(?<key>[A-Za-z_][A-Za-z0-9_]*)[[:space:]]*=(?<value>.*)$")) catch null) as $match
          | if $match == null then bad(".env: invalid assignment at line " + (($entry.key + 1) | tostring))
            else .[$match.key] = dotenv_value($match.value; $entry.key + 1)
            end
        end
    );
  def required_env($values; $name; $context):
    if ($name | valid_env_name | not) then bad($context + ": invalid environment variable name")
    elif (($values[$name] // "") | type) != "string" or (($values[$name] // "") | length) == 0
      then bad($context + ": missing required environment variable " + $name)
    else $values[$name]
    end;
  def required_port($values; $name; $context):
    (required_env($values; $name; $context)) as $raw
    | (try ($raw | tonumber) catch bad($context + ": port must be an integer")) as $port
    | if ($port | floor) != $port or $port < 1 or $port > 65535
      then bad($context + ": port must be in the range 1..65535") else $port end;
  def require_array($value; $context):
    if ($value | type) != "array" then bad($context + " must be an array") else $value end;

  ($base[0]) as $base_config
  | ($inventory[0]) as $inventory_config
  | ($group_policy[0]) as $group_config
  | ($route_policy[0]) as $route_config
  | if $inventory_config.version != 1 or $group_config.version != 1 or $route_config.version != 1
    then bad("unsupported inventory or policy version") else . end
  | (require_array($inventory_config.nodes; "inventory.nodes")) as $nodes
  | (require_array($group_config.groups; "policy.groups")) as $groups
  | (require_array($route_config.routes; "policy.routes")) as $routes_unsorted
  | if ($nodes | length) == 0 or ($groups | length) == 0 or ($routes_unsorted | length) == 0
    then bad("nodes, groups, and routes must be non-empty") else . end
  | if any($nodes[]; (.tag | valid_tag | not) or .type != "shadowsocks")
    then bad("inventory.nodes: invalid tag or unsupported node type") else . end
  | if ([$nodes[].tag] | unique_values | not) then bad("inventory.nodes: duplicate node tag") else . end
  | if any($groups[];
      (.tag | valid_tag | not)
      or (.urltest.tag | valid_tag | not)
      or has("dns")
      or ((.members | type) != "array")
      or (.members | length) == 0
      or any(.members[]; (type != "string")))
    then bad("policy.groups: invalid group, urltest, or members; per-group dns is no longer supported") else . end
  | if ([$groups[].tag] | unique_values | not) or ([$groups[].urltest.tag] | unique_values | not)
    then bad("policy.groups: duplicate generated tag") else . end
  | ([$nodes[].tag]) as $node_tags
  | if any($groups[]; any(.members[]; . as $member | ($node_tags | index($member)) == null))
    then bad("policy.groups: member references an unknown node") else . end
  | if any($groups[]; .default as $default | (([.urltest.tag] + .members) | index($default)) == null)
    then bad("policy.groups: default must reference its urltest or a member") else . end
  | if any($routes_unsorted[];
      (.tag | valid_tag | not)
      or (.priority | type) != "number"
      or (.priority | floor) != .priority
      or .priority < 0
      or has("dns")
      or (.source | type) != "string")
    then bad("policy.routes: invalid route; per-route dns is no longer supported") else . end
  | if ([$routes_unsorted[].tag] | unique_values | not)
      or ([$routes_unsorted[].source] | unique_values | not)
      or ([$routes_unsorted[].priority] | unique_values | not)
    then bad("policy.routes: duplicate tag, source, or priority") else . end
  | ([$groups[].tag]) as $group_tags
  | if any($routes_unsorted[]; .outbound as $outbound | ($group_tags | index($outbound)) == null)
    then bad("policy.routes: outbound references an unknown group") else . end
  | (parse_dotenv + env) as $values
  | (if $ruleset_dir == "" then
      (required_env($values; "TARGET_CONFIG_DIR"; "render config")) as $target_config_dir
      | if ($target_config_dir | startswith("/") | not) or $target_config_dir == "/"
        then bad("render config: TARGET_CONFIG_DIR must be an absolute non-root directory")
        else (($target_config_dir | sub("/+$"; "")) + "/ruleset")
        end
    else $ruleset_dir end) as $effective_ruleset_dir
  | ($nodes | map(
      . as $node
      | {
          type: $node.type,
          tag: $node.tag,
          server: required_env($values; $node.server_env; "node " + $node.tag),
          server_port: required_port($values; $node.server_port_env; "node " + $node.tag),
          method: required_env($values; $node.method_env; "node " + $node.tag),
          password: required_env($values; $node.password_env; "node " + $node.tag)
        }
        + (if ($node.connect_timeout // "") != "" then {connect_timeout: $node.connect_timeout} else {} end)
    )) as $physical_outbounds
  | ($groups | map({
      type: "urltest",
      tag: .urltest.tag,
      outbounds: .members,
      url: .urltest.url,
      interval: .urltest.interval,
      tolerance: .urltest.tolerance,
      idle_timeout: .urltest.idle_timeout,
      interrupt_exist_connections: false
    })) as $urltest_outbounds
  | ($groups | map({
      type: "selector",
      tag: .tag,
      outbounds: ([.urltest.tag] + .members),
      default: .default,
      interrupt_exist_connections: false
    })) as $selector_outbounds
  | ($routes_unsorted | sort_by(.priority)) as $routes
  | ($routes | map({rule_set: .tag, action: "route", outbound: .outbound})) as $route_rules
  | ($routes | map({tag: .tag, type: "local", format: "binary", path: ($effective_ruleset_dir + "/" + .tag + ".srs")})) as $rule_sets
  | (($base_config.outbounds // []) | map(.tag)) as $base_outbound_tags
  | (($base_config.dns.servers // []) | map(.tag)) as $base_dns_tags
  | if ($base_outbound_tags + $node_tags + [$groups[].urltest.tag] + $group_tags | unique_values | not)
    then bad("generated outbound tag collides with a base or generated tag") else . end
  | if ($base_dns_tags | length) != 1 or $base_dns_tags[0] != "dns-direct"
    then bad("base config must define exactly one DNS server tagged dns-direct") else . end
  | if (($base_config.dns.rules // []) | length) != 0
    then bad("base config dns.rules must be empty when using unified DNS") else . end
  | ($base_config.dns.servers[0]) as $default_dns
  | if $default_dns.type != "https" then bad("base config DNS server dns-direct must use HTTPS") else . end
  | if $default_dns.server != "1.1.1.1" or ($default_dns | has("detour"))
    then bad("base config DNS server dns-direct must use the default dialer with a literal server IP") else . end
  | if $base_config.dns.final != "dns-direct" then bad("base config dns.final must remain dns-direct") else . end
  | if $base_config.route.default_domain_resolver != "dns-direct"
    then bad("base config route.default_domain_resolver must remain dns-direct") else . end
  | $base_config
  | .outbounds = ((.outbounds // []) + $physical_outbounds + $urltest_outbounds + $selector_outbounds)
  | .route.rules = ((.route.rules // []) + $route_rules)
  | .route.rule_set = ((.route.rule_set // []) + $rule_sets)
' > "$temp_file"

jq -e 'type == "object"' "$temp_file" >/dev/null
require_safe_output_target
chmod 600 "$temp_file"
mv -f -- "$temp_file" "$output_file"
temp_file=""
trap - EXIT INT TERM
printf 'rendered config to %s\n' "$output_file"
