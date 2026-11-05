#!/usr/bin/env bash
set -euo pipefail
umask 077

usage() {
    printf 'Usage: %s [shared-directory]\n' "${0##*/}"
    printf 'Move .env and config.yaml into Dropbox and link them into this project.\n'
    printf 'Default shared directory: $HOME/Dropbox/vpn-monitor\n'
    printf 'On another machine, sync Dropbox first, then run this script.\n'
}

fail() {
    printf 'Error: %s\n' "$*" >&2
    exit 1
}

if [[ ${1:-} == --help || ${1:-} == -h ]]; then
    usage
    exit 0
fi
[[ $# -le 1 ]] || { usage >&2; exit 1; }

project_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
if [[ $# -eq 0 ]]; then
    [[ -d "$HOME/Dropbox" ]] || fail 'Dropbox directory not found; pass its shared directory explicitly.'
fi
shared_dir=${1:-"$HOME/Dropbox/vpn-monitor"}
[[ -n "$shared_dir" ]] || fail 'Shared directory must not be empty.'
mkdir -p -- "$shared_dir"
shared_dir=$(cd -- "$shared_dir" && pwd -P)
[[ "$shared_dir" != "$project_dir" ]] || fail 'Shared directory must differ from the project directory.'

# Check both files before moving either; never choose between conflicting copies.
for name in .env config.yaml; do
    local_file="$project_dir/$name"
    shared_file="$shared_dir/$name"
    if [[ -e "$shared_file" || -L "$shared_file" ]]; then
        [[ -f "$shared_file" && ! -L "$shared_file" ]] || fail "$shared_file must be a regular file, not a symlink."
        if [[ -e "$local_file" || -L "$local_file" ]]; then
            [[ "$local_file" -ef "$shared_file" ]] || fail "Conflicting $name files. Back up and reconcile them manually, then remove the local copy."
        fi
    else
        [[ ! -L "$local_file" ]] || fail "$local_file is a symlink but $shared_file is missing. Check the path and wait for Dropbox to sync."
        [[ -f "$local_file" ]] || fail "$name is missing from both locations. Create it locally or wait for Dropbox to sync."
    fi
done

for name in .env config.yaml; do
    local_file="$project_dir/$name"
    shared_file="$shared_dir/$name"
    if [[ ! -e "$shared_file" ]]; then
        mv -n -- "$local_file" "$shared_file"
        [[ ! -e "$local_file" ]] || fail "Could not move $local_file without overwriting a shared file."
    fi
    chmod 600 -- "$shared_file"
    if [[ ! -e "$local_file" && ! -L "$local_file" ]]; then
        ln -s -- "$shared_file" "$local_file"
    fi
    printf '%s -> %s\n' "$local_file" "$shared_file"
done

printf '\nSetup complete. Keep data/ local and review machine-specific config paths.\n'
printf 'Secrets are now stored in Dropbox. Only source a trusted, shell-compatible .env.\n'
printf 'The app does not load .env automatically. From the project directory, run:\n\n'
printf '  set -a\n  . ./.env\n  set +a\n  bin/vpn-monitor --config config.yaml\n'
