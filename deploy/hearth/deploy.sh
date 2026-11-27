#!/usr/bin/env bash
# Deploy HEAD to hearth (the iac repo's shared host): cross-build serene for arm64, upload a
# release to s3://serene-<account>/releases/, then unpack it and run host.sh there through
# Systems Manager. Needs `iac serene up` once and deploy/hearth/env.sh for .env.
set -euo pipefail
cd "$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
export AWS_PROFILE=${AWS_PROFILE:-iac} AWS_REGION=${AWS_REGION:-ap-south-1}

[[ -z $(git status --porcelain --untracked-files=no) ]] || { echo "commit first: deploy ships HEAD" >&2; exit 1; }
rev=$(git rev-parse --short HEAD)

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir "$work/release"
# DuckDB is cgo: its arm64 static libraries come with go.duckdb.org/libduckdb/linux-arm64, the C++
# toolchain from g++-aarch64-linux-gnu.
command -v aarch64-linux-gnu-g++ > /dev/null ||
  { echo "no arm64 cross-compiler: sudo apt install g++-aarch64-linux-gnu" >&2; exit 1; }
CGO_ENABLED=1 GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc CXX=aarch64-linux-gnu-g++ \
  go build -trimpath -ldflags '-s -w' -o "$work/release/serene" ./cmd/serene
cp deploy/hearth/host.sh "$work/release/"
mkdir "$work/release/configs" && cp configs/costs_mcx.yaml configs/costs.yaml "$work/release/configs/"
shopt -s nullglob # no units: host.sh removes the installed ones
for u in deploy/hearth/serene-*.service deploy/hearth/serene-*.timer; do cp "$u" "$work/release/"; done
shopt -u nullglob
echo "$rev" > "$work/release/REVISION"
tar -C "$work/release" -czf "$work/release.tar.gz" .

account=$(aws sts get-caller-identity --query Account --output text)
bucket=serene-$account key=releases/$rev.tar.gz
# A slow or flaky uplink makes the default CRT client time out on the ~30 MB release, so upload it
# with the classic client in 5 MB parts, one at a time, retrying each. The settings go on a copy of
# the AWS config, so the user's own config is left alone.
cfg=${AWS_CONFIG_FILE:-$HOME/.aws/config}
[[ -f $cfg ]] && cp "$cfg" "$work/aws_config" || : > "$work/aws_config"
for kv in s3.preferred_transfer_client=classic s3.max_concurrent_requests=1 \
  s3.multipart_threshold=5MB s3.multipart_chunksize=5MB max_attempts=15 retry_mode=standard; do
  AWS_CONFIG_FILE=$work/aws_config aws configure set "${kv%%=*}" "${kv#*=}"
done
AWS_CONFIG_FILE=$work/aws_config aws s3 cp "$work/release.tar.gz" "s3://$bucket/$key" --only-show-errors
echo "uploaded $key ($(du -h "$work/release.tar.gz" | cut -f1))"

instance=$(aws ec2 describe-instances --filters Name=tag:Name,Values=hearth Name=instance-state-name,Values=running \
  --query 'Reservations[].Instances[].InstanceId' --output text)
[[ $instance =~ ^i-[0-9a-f]+$ ]] || { echo "hearth isn't running (found '$instance')" >&2; exit 1; }

script="set -e; d=\$(mktemp -d); /snap/bin/aws --region $AWS_REGION s3 cp s3://$bucket/$key \$d/r.tgz --only-show-errors; tar -C \$d -xzf \$d/r.tgz; bash \$d/host.sh; rm -rf \$d"
id=$(aws ssm send-command --instance-ids "$instance" --document-name AWS-RunShellScript \
  --comment "serene deploy $rev" --parameters "$(jq -cn --arg s "$script" '{commands: [$s]}')" \
  --query Command.CommandId --output text)

for _ in $(seq 120); do
  sleep 3
  status=$(aws ssm get-command-invocation --command-id "$id" --instance-id "$instance" --query Status --output text 2> /dev/null || echo Pending)
  [[ $status == Pending || $status == InProgress || $status == Delayed ]] || break
done
aws ssm get-command-invocation --command-id "$id" --instance-id "$instance" --query StandardOutputContent --output text
err=$(aws ssm get-command-invocation --command-id "$id" --instance-id "$instance" --query StandardErrorContent --output text)
[[ -z $err || $err == None ]] || echo "$err" >&2
[[ $status == Success ]] || { echo "deploy $rev: $status" >&2; exit 1; }
