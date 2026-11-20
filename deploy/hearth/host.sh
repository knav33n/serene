#!/usr/bin/env bash
# Runs on hearth as root (deploy.sh sends it over Systems Manager) from the unpacked release:
# installs the binary, writes .env from /serene/env, lets serene write project_calm's broker
# logins, installs and starts the timers and services it ships (removing any it no longer does),
# and makes the manual trades page (serene-manual) the home page of hearth's Caddy site. The iac
# serene stack's setup.sh made the user, /home/serene/app and the database.
set -euo pipefail
cd "$(dirname "$0")"
app=/home/serene/app
region=$(cat /etc/serene/region)

install -m 755 -o serene -g serene serene "$app/serene.new"
mv "$app/serene.new" "$app/serene"
install -d -o serene -g serene "$app/data/mcx" "$app/configs"
install -m 644 -o serene -g serene configs/* "$app/configs/"

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
grant select, insert, update on trading.broker_sessions to serene;
SQL

shopt -s nullglob
# Units dropped from the release are stopped and removed.
for path in /etc/systemd/system/serene-*.service /etc/systemd/system/serene-*.timer; do
  u=$(basename "$path")
  [[ -f $u ]] && continue
  systemctl disable --now -q "$u" || true
  rm -f "$path"
  echo "removed $u"
done
for u in serene-*.service serene-*.timer; do install -m 644 "$u" /etc/systemd/system/; done
systemctl daemon-reload
for t in serene-*.timer; do systemctl enable --now -q "$t"; done
# Long-running services (an [Install] section) restart on the new binary and .env.
for u in serene-*.service; do
  grep -q '^\[Install\]' "$u" || continue
  systemctl enable -q "$u"
  systemctl restart "$u"
done

# The manual page is every path no other project's /etc/caddy/hearth/*.caddy claims: their
# handle_path /<project>/* blocks match first, this unmatched handle takes the rest (the home page).
site=/etc/caddy/hearth/serene.caddy
want="# Written by serene's deploy/hearth/host.sh: the manual trades page (serene-manual) at /
handle {
	reverse_proxy 127.0.0.1:8767
}"
if [[ "$(cat $site 2> /dev/null)" != "$want" ]]; then
  [[ ! -f $site ]] || cp $site $site.old
  printf '%s\n' "$want" > $site
  if ! caddy validate --adapter caddyfile --config /etc/caddy/Caddyfile > /dev/null 2>&1; then
    if [[ -f $site.old ]]; then mv $site.old $site; else rm $site; fi
    echo "error: Caddy refused serene's home page" >&2; exit 1
  fi
  rm -f $site.old
  systemctl restart caddy # admin is off, so no reload
  echo "wrote $site"
fi
echo "deployed $(cat REVISION)"
systemctl list-timers --no-pager 'serene-*'
