#!/usr/bin/env bash
# rbash-init.sh — Create a minimal symlink directory for restricted bash (rbash).
#
# Usage: ./cmd/rbash-init.sh [--dir DIR]
#
# Creates a directory with symlinks to only the allowed binaries.
# Interpreters (python, node, perl, etc.) are explicitly excluded.
# Binaries that don't exist on the host are silently skipped.
#
# After running, configure hufu with:
#   restricted-path: ~/.rbash-bin
# And run with:
#   hufu --rbash "your prompt"

set -euo pipefail

RBASH_DIR="${HOME}/.rbash-bin"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dir)
      RBASH_DIR="$2"
      shift 2
      ;;
    *)
      echo "Unknown option: $1" >&2
      exit 1
      ;;
  esac
  shift
done

# Allowed binaries for agents (no interpreters, no destructive tools, no shells)
ALLOWED="ls cat mkdir rmdir cp mv ln chmod touch find grep sed awk head tail sort
uniq wc cut tr diff patch tar gzip gunzip bzip2 bunzip2 xz unxz zstd zcat
bzcat xzcat curl wget git jq yq sha1sum sha256sum md5sum base64
xargs tee date sleep echo printf test true false realpath readlink dirname
basename stat file du df free ps lsof env printenv nohup timeout flock
docker nm objdump strip ar ranlib strings size seq whoami hostname uname id
git-receive-pack git-upload-pack git-upload-archive unzip flock"

# Explicitly excluded — interpreters, shells, package managers, dangerous tools
EXCLUDED="python python3 pip pip3 pipx node npm npx pnpm yarn ruby irb gem
perl tclsh wish lua luac java javac bc sh bash dash csh tcsh zsh fish ksh
mksh rbash ash go golangci-lint ollama nvim starship vim vi nano
rm make gcc g++ ssh scp rsync install"

excluded_map=""
for e in $EXCLUDED; do
  excluded_map="$excluded_map $e"
done

is_excluded() {
  for e in $excluded_map; do
    if [[ "$1" == "$e" ]]; then
      return 0
    fi
  done
  return 1
}

mkdir -p "$RBASH_DIR"

created=0
skipped=0
excluded_count=0

for cmd in $ALLOWED; do
  if is_excluded "$cmd"; then
    excluded_count=$((excluded_count + 1))
    continue
  fi

  src="$(command -v "$cmd" 2>/dev/null || true)"
  if [[ -z "$src" ]]; then
    skipped=$((skipped + 1))
    continue
  fi

  ln -sf "$src" "$RBASH_DIR/$cmd" 2>/dev/null || true
  created=$((created + 1))
done

echo "rbash-bin directory: $RBASH_DIR"
echo "Symlinks created:    $created"
echo "Skipped (not found): $skipped"
echo "Excluded:            $excluded_count"
echo ""
echo "Add to hufu.yaml or team.yaml:"
echo "  restricted-path: $RBASH_DIR"