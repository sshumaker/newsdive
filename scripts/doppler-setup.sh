#!/usr/bin/env bash
# One-time Doppler setup for local development.
#
# Usage:
#   doppler login            # interactive browser auth (do this first)
#   ./scripts/doppler-setup.sh
#
# On a second machine (e.g. your laptop), the secret already lives in Doppler,
# so you only need:  doppler login && doppler setup -p newsdive -c dev
set -euo pipefail

PROJECT=newsdive
CONFIG=dev

if ! command -v doppler >/dev/null 2>&1; then
  echo "Doppler CLI not found. Install: brew install dopplerhq/cli/doppler" >&2
  exit 1
fi
if ! doppler me >/dev/null 2>&1; then
  echo "Not logged in. Run 'doppler login' first, then re-run this script." >&2
  exit 1
fi

# Create the project (idempotent — ignore 'already exists').
doppler projects create "$PROJECT" >/dev/null 2>&1 && echo "Created project '$PROJECT'." \
  || echo "Project '$PROJECT' already exists."

# Associate this directory with project/config (writes to ~/.doppler, not the repo).
doppler setup --no-interactive --project "$PROJECT" --config "$CONFIG" >/dev/null
echo "This directory is now scoped to $PROJECT/$CONFIG."

# Seed secrets from .env the first time (no value is printed).
if [ -f .env ]; then
  doppler secrets upload .env >/dev/null
  echo "Uploaded secrets from .env into $PROJECT/$CONFIG."
fi

# Verify the key is available (without printing it).
if doppler run --command 'test -n "${ANTHROPIC_API_KEY:-}"' >/dev/null 2>&1; then
  echo "✓ Ready. Run the app with:  doppler run -- ./bin/news serve"
  echo "  (Once verified, you can delete the local .env — the key lives in Doppler now.)"
else
  echo "! ANTHROPIC_API_KEY is not in Doppler yet. Add it with:"
  echo "    doppler secrets set ANTHROPIC_API_KEY"
fi
