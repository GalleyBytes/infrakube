#!/bin/bash -e
set -o errexit

function fixssh {
  mkdir -p /tmp/.ssh/
  if stat "$INFRAKUBE_SSH"/* >/dev/null 2>/dev/null; then
    cp -Lr "$INFRAKUBE_SSH"/* /tmp/.ssh/
    chmod -R 0600 /tmp/.ssh/*
  fi
}

function join_by {
  local d="$1" f=${2:-$(</dev/stdin)};
  if [[ -z "$f" ]]; then return 1; fi
  if shift 2; then
    printf %s "$f" "${@/#/$d}"
  else
    join_by "$d" $f
  fi
}

function version_gt_or_eq {
  if [ "$(printf '%s\n' "$1" "$2" | sort -V | head -n1)" = "$1" ]; then
    return 0
  else
    return 1
  fi
}

# Start by fixing ssh in case another task left it in a bad state
fixssh

if [[ -s "$AWS_WEB_IDENTITY_TOKEN_FILE" ]] && [[ -n "$AWS_ROLE_ARN" ]]; then
  temp="$(mktemp)"
  irsa-tokengen > "$temp" || exit $?
  export $(cat "$temp")
fi
tofu_version=$(tofu version | head -n1 | sed "s/^.*v//")
module=""
if ! version_gt_or_eq "0.15.0" "$tofu_version"; then
  module="."
fi

cd "$INFRAKUBE_MAIN_MODULE"
out="$INFRAKUBE_ROOT_PATH"/generations/$INFRAKUBE_GENERATION
mkdir -p "$out"
vardir="$out/tfvars"
vars=
if [[ $(ls $vardir | wc -l) -gt 0 ]]; then
  vars="-var-file $(find $vardir -type f | sort -n | join_by ' -var-file ')"
fi

case "$INFRAKUBE_TASK" in
    init | init-delete)
        tofu init $module 2>&1
        ;;
    plan)
        tofu plan $vars -out tfplan $module 2>&1
        ;;
    plan-delete)
        tofu plan $vars -destroy -out tfplan $module 2>&1
        ;;
    apply | apply-delete)
        tofu apply tfplan 2>&1
        ;;
esac
status=${PIPESTATUS[0]}
if [[ $status -gt 0 ]];then exit $status;fi

if [[ "$INFRAKUBE_TASK" == "apply" ]] && [[ "$INFRAKUBE_SAVE_OUTPUTS" == "true" ]]; then
  data=$(mktemp)
  printf '[
    {"op":"replace","path":"/data","value":{}}
  ]' > "$data"
  t=$(mktemp)
  include=( $(echo "$INFRAKUBE_OUTPUTS_TO_INCLUDE" | tr "," " ") )
  omit=( $(echo "$INFRAKUBE_OUTPUTS_TO_OMIT" | tr "," " ") )
  jsonoutput=$(tofu output -json)
  keys=( $(jq -r '.|keys[]' <<< $jsonoutput) )
  for key in ${keys[@]}; do
    if [[ "${#include[@]}" -gt 0 ]] && [[ ! " ${include[*]} " =~ " $key " ]]; then
      echo "Skipping $key"
      continue
    fi
    if [[ "${#omit[@]}" -gt 0 ]] && [[ " ${omit[*]} " =~ " $key " ]]; then
      echo "Omitting $key"
      continue
    fi
    b64value=$(jq -j --arg key $key '.[$key].value' <<< $jsonoutput|base64|tr -d '[:space:]')
    jq -Mc --arg key "$key" --arg value "$b64value" '. += [
      {"op":"add","path":"/data/\($key)","value":"\($value)"}
    ]' "$data" > "$t"
    cp "$t" "$data"
  done
  kubectl patch secret "$INFRAKUBE_OUTPUTS_SECRET_NAME" --type json --patch-file "$data"
fi
