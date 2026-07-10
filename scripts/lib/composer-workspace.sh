#!/usr/bin/env bash
# composer-workspace.sh — shared capture function for all composer harnesses
# Workspace is an advisory hint. A launcher failure stops that launcher, while
# headerless harnesses remain fully supported by the proxy and bridge.

composer_capture_workspace() {
  local cwd
  if ! cwd=$(pwd -P 2>/dev/null); then
    echo "composer_capture_workspace: failed to get physical cwd (pwd -P)" >&2
    return 1
  fi
  if [[ -z "$cwd" ]]; then
    echo "composer_capture_workspace: empty cwd" >&2
    return 1
  fi
  # Reject control characters in cwd (NUL cannot be represented in bash)
  if [[ "$cwd" == *$'\n'* || "$cwd" == *$'\r'* ]]; then
    echo "composer_capture_workspace: cwd contains control characters" >&2
    return 1
  fi

  local workspace
  if [[ -n "${CLIPROXY_WORKSPACE_PATH:-}" ]]; then
    workspace="${CLIPROXY_WORKSPACE_PATH}"
    if [[ -z "$workspace" ]]; then
      echo "composer_capture_workspace: CLIPROXY_WORKSPACE_PATH is empty" >&2
      return 1
    fi
  else
    local git_root
    if git_root=$(git -C "$cwd" rev-parse --show-toplevel 2>/dev/null) && [[ -n "$git_root" ]]; then
      if ! workspace=$(cd -- "$git_root" && pwd -P 2>/dev/null); then
        echo "composer_capture_workspace: failed to resolve git root $git_root" >&2
        return 1
      fi
    else
      workspace="$cwd"
    fi
  fi

  if [[ -z "$workspace" ]]; then
    echo "composer_capture_workspace: empty workspace" >&2
    return 1
  fi
  if [[ "$workspace" == *$'\n'* || "$workspace" == *$'\r'* ]]; then
    echo "composer_capture_workspace: workspace contains control characters" >&2
    return 1
  fi


  local shell_name
  shell_name="${SHELL:-bash}"
  shell_name=$(basename "$shell_name")

  local os_version
  os_version=$(uname -r 2>/dev/null || echo "unknown")

  export CLIPROXY_CLIENT_CWD="$cwd"
  export CLIPROXY_CLIENT_WORKSPACE="$workspace"
  export CLIPROXY_CLIENT_SHELL="$shell_name"
  export CLIPROXY_CLIENT_OS_VERSION="$os_version"
}
