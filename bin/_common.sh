# Shared helpers, sourced by every bin/ script.
set -euo pipefail

ROOT="$(cd "$(dirname "$(realpath "${BASH_SOURCE[0]}")")/.." && pwd)"
STATE_DIR="$ROOT/.state"
# shellcheck source=/dev/null
source "$ROOT/vast-coder.env"
umask 077
mkdir -p "$STATE_DIR"
chmod 700 "$STATE_DIR"

# Resolve credentials only for commands that actually contact Vast.
vast() {
  if [[ -z "${VAST_API_KEY:-}" ]]; then
    VAST_API_KEY="$(op read "$VAST_KEY_REF")"
  fi
  command vastai --api-key "$VAST_API_KEY" "$@"
}

die() { echo "error: $*" >&2; exit 1; }

# Prints the instance id for $LABEL, or nothing if none exists.
instance_id() {
  vast show instances --raw | python3 -c '
import json, sys
label = sys.argv[1]
ids = [str(i["id"]) for i in json.load(sys.stdin) if i.get("label") == label]
if len(ids) > 1:
    sys.exit("multiple instances labelled %s: %s" % (label, ", ".join(ids)))
print(ids[0] if ids else "")
' "$LABEL"
}

# Bearer token for the model server. Generated once, kept out of git.
llm_api_key() {
  local f="$STATE_DIR/llm_api_key"
  if [[ ! -s "$f" ]]; then
    (umask 077; openssl rand -hex 24 > "$f")
  fi
  cat "$f"
}
