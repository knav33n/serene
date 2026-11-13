#!/usr/bin/env bash
# Upload deploy/hearth/server.env (git-ignored) to the SSM SecureString /serene/env, which each
# deploy writes to hearth's /home/serene/app/.env. Run deploy.sh afterwards to apply it.
set -euo pipefail
cd "$(dirname "$0")"
export AWS_PROFILE=${AWS_PROFILE:-iac} AWS_REGION=${AWS_REGION:-ap-south-1}

[[ -f server.env ]] || { echo "no deploy/hearth/server.env: copy server.env.example and fill it in" >&2; exit 1; }
for k in SERENE_DATABASE_URL CALM_DATABASE_URL SERENE_TOKEN_KEY ZERODHA_API_KEY SLACK_TOKEN SLACK_CHANNEL SLACK_TRADES_CHANNEL SLACK_SILVERM_CHANNEL SLACK_PAPER_CHANNEL SLACK_PDL_CHANNEL SLACK_ALERT_CHANNEL; do
  grep -qE "^$k=.+" server.env || { echo "server.env: $k is empty" >&2; exit 1; }
done
aws ssm put-parameter --name /serene/env --type SecureString --overwrite \
  --value "file://server.env" --output text > /dev/null
echo "wrote /serene/env; next: deploy/hearth/deploy.sh"
