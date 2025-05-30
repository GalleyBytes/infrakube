#!/bin/bash -e
set -o errexit

# Expects all scripts configure ssh in /tmp/.ssh
rm -rf ~/.ssh || true
ln -s /tmp/.ssh ~/.ssh
# Setup SSH
mkdir -p /tmp/.ssh/
if stat "$I3_SSH"/* >/dev/null 2>/dev/null; then
    cp -Lr "$I3_SSH"/* /tmp/.ssh/
    chmod -R 0600 /tmp/.ssh/*
fi

out="$I3_ROOT_PATH"/generations/$I3_GENERATION
vardir="$out/tfvars"
if [[ "$I3_CLEANUP_DISK" == "true" ]]; then
    rm -rf "$I3_ROOT_PATH"/generations/* || true
fi
mkdir -p "$out"
mkdir -p "$vardir"

if [[ -d "$I3_MAIN_MODULE" ]]; then
    rm -rf "$I3_MAIN_MODULE" || true
fi

if [[ ! -s "$I3_MAIN_MODULE_ADDONS/inline-module.tf" ]]; then
    # The inline module is not defined or is empty and has to be fetched or
    # copied from another configmap

    configmap="$I3_MAIN_MODULE_ADDONS/.__I3__ConfigMapModule.json"
    if [[ -s $configmap ]]; then
        # When downloading the module from a configmap, the I3_MAIN_MODULE dir
        # must first be created to coppy the contents of the configmap into.
        mkdir -p "$I3_MAIN_MODULE"

        name=$(jq -r '.name' "$configmap")
        configmap_json=$(kubectl get configmap --namespace "$I3_NAMESPACE" "$name" -ojson)

        key=$(jq -r '.key//empty' "$configmap")
        if [[ -n "$key" ]]; then
            # The key is defined and must be a tf file. Check or Create a
            # file type suffix
            suffix=
            if [[ "$key" != *".tf" ]] || [[ "$key" != *".json" ]]; then
                suffix=".tf" # select tf as default
            fi
            jq -r --arg key "$key" '.data[$key]' <<<$configmap_json >"${I3_MAIN_MODULE}/${key}${suffix}"
        else
            for key in $(jq -r '.data | keys[]' <<<$configmap_json); do
                # No assumptions about the file types are made here. The user
                # should create keys that are properly suffixed for tf.
                jq -r --arg key "$key" '.data[$key]' <<<$configmap_json >"${I3_MAIN_MODULE}/${key}"
            done
        fi
    # Check if this is a source directory instead of a git repo
    elif [[ "$I3_MAIN_MODULE_REPO" == file://* ]]; then
        local_module_path="${I3_MAIN_MODULE_REPO#"file://"}"
        if [[ -d "$local_module_path" ]]; then
            cp -r "$local_module_path" "$I3_MAIN_MODULE"
        else
            echo "tf module file source: $local_module_path, does not exist"
            exit 1
        fi
    else
        # The tf module is a repo that must be downloaded
        cd $(mktemp -d)
        git clone "$I3_MAIN_MODULE_REPO" 2>&1 | tee .out
        exit_code=${PIPESTATUS[0]}
        if [ $exit_code -ne 0 ]; then
            exit $exit_code
        fi
        reponame=$(sed -n "s/.*'\([^']*\)'.*/\1/p" .out | head -n1)
        cd "$reponame"
        git checkout "$I3_MAIN_MODULE_REPO_REF"
        echo "Setting up module for $reponame/$I3_MAIN_MODULE_REPO_SUBDIR"
        cp -r "$I3_MAIN_MODULE_REPO_SUBDIR" "$I3_MAIN_MODULE"
    fi
fi

# Get configmap and secret files and drop them in the main module's root path.
# Will not copy over "hidden" files (files that begin with '.').
# Do not overwrite configmap
mkdir -p $I3_MAIN_MODULE
false | cp -iLr "$I3_MAIN_MODULE_ADDONS"/* "$I3_MAIN_MODULE" 2>/dev/null || true

cd "$I3_MAIN_MODULE"

# Load a custom backend
if stat backend_override.tf >/dev/null 2>/dev/null; then
    echo "Using custom backend"
else
    echo "Loading hashicorp backend"
    set -x
    envsubst </backend.tf >"$I3_ROOT_PATH/backend_override.tf"
    mv "$I3_ROOT_PATH/backend_override.tf" .
fi

function join_by {
    local d="$1" f=${2:-$(</dev/stdin)}
    if [[ -z "$f" ]]; then return 1; fi
    if shift 2; then
        printf %s "$f" "${@/#/$d}"
    else
        join_by "$d" $f
    fi
}

function add_file_as_next_index {
    dir="$1"
    file="$2"
    idx=$(ls "$dir" | wc -l)
    cp "$file" "$dir/${idx}_$(basename $2)"
}

function fetch_git {
    temp="$(mktemp -d)"
    repo="$1"
    relpath="$2"
    files="$3"
    tfvar="$4"
    branch="$5"
    path="$I3_MAIN_MODULE/$relpath"
    if [[ "$files" == "." ]] && ([[ "$relpath" == "." ]] || [[ "$relpath" == "" ]]); then
        a=$(basename $repo)
        path="$I3_MAIN_MODULE/${a%.*}"
    fi
    # printf -- 'mkdir -p "'$path'"\n'
    mkdir -p "$path"
    # printf -- 'git clone "'$repo'" "'$temp'"\n'
    echo "Downloading resources from $repo"
    git clone "$repo" "$temp"
    cd "$temp" # All files are relative to the root of the repo
    git checkout "$branch"
    # printf -- "cp -r $files $path\n"
    cp -r $files $path

    if [[ "$tfvar" == "true" ]]; then
        for file in $files; do
            if [[ -f "$file" ]]; then
                # printf 'add_file_as_next_index "'$vardir'" "'$file'"'
                add_file_as_next_index "$vardir" "$file"
            fi
        done
    fi
    echo done
}

FILE="$I3_MAIN_MODULE_ADDONS/.__I3__ResourceDownloads.json"
LENGTH=$(jq '.|length' $FILE)

for i in $(seq 0 $((LENGTH - 1))); do
    DATA=$(mktemp)
    jq --argjson i $i '.[$i]' $FILE >$DATA
    fetchtype=$(jq -r '.detect' $DATA)
    repo=$(jq -r '.repo' $DATA)
    files=$(jq -r '.files[]' $DATA | join_by "  ")
    path=$(jq -r '.path' $DATA)
    tfvar=$(jq -r '.useAsVar' $DATA)
    branch=$(jq -r '.hash' $DATA)
    if [[ "$fetchtype" == "git" ]]; then
        fetch_git "$repo" "$path" "$files" "$tfvar" "$branch"
    fi
done
