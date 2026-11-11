#!/usr/bin/env bash
# Runs on hearth as root (deploy.sh sends it over Systems Manager) from the unpacked release:
# installs the binary, writes .env from /serene/env, lets serene read project_calm's broker
# logins, and installs and starts the timers. The iac serene stack's setup.sh made the user,
# /home/serene/app and the database.
set -euo pipefail
cd "$(dirname "$0")"
app=/home/serene/app
region=$(cat /etc/serene/region)

install -m 755 -o serene -g serene serene "$app/serene.new"
mv "$app/serene.new" "$app/serene"
install -d -o serene -g serene "$app/data/mcx"

# Read through $(...): the snap aws's output redirected into a /tmp file comes out empty.
value=$(/snap/bin/aws --region "$region" ssm get-parameter --name /serene/env --with-decryption \
  --query Parameter.Value --output text)
grep -q '^SERENE_TOKEN_KEY=.' <<< "$value" || { echo "/serene/env has no SERENE_TOKEN_KEY: deploy/hearth/env.sh" >&2; exit 1; }
install -m 600 -o serene -g serene /dev/null "$app/.env"
printf '%s\n' "$value" > "$app/.env"

# Again on every deploy: calm infra restore recreates project_calm, which drops the grants.
sudo -u postgres psql -v ON_ERROR_STOP=1 -q -d project_calm <<'SQL'
grant connect on database project_calm to serene;
grant usage on schema trading to serene;
grant select on trading.broker_sessions to serene;
SQL

for u in serene-*.service serene-*.timer; do install -m 644 "$u" /etc/systemd/system/; done
systemctl daemon-reload
for t in serene-*.timer; do systemctl enable --now -q "$t"; done
echo "deployed $(cat REVISION)"
systemctl list-timers --no-pager 'serene-*'
