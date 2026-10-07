#!/bin/sh
# Starts the pinned PLANKA for the benchmarks. Remove it all with:
#   docker compose down -v
set -eu
cd "$(dirname "$0")"
if [ ! -f .env ]; then
	{
		echo "PLANKA_SECRET_KEY=$(openssl rand -hex 64)"
		echo "PLANKA_DB_PASSWORD=$(openssl rand -hex 24)"
		echo "PLANKA_ADMIN_PASSWORD=$(openssl rand -hex 12)"
	} > .env
	chmod 600 .env
fi
docker compose up -d
. ./.env
for i in $(seq 1 90); do
	if curl -sf -o /dev/null "http://${PLANKA_BIND:-172.17.0.1}:3300/"; then
		echo "PLANKA is up on http://${PLANKA_BIND:-172.17.0.1}:3300"
		exit 0
	fi
	sleep 2
done
echo "PLANKA did not answer" >&2
docker compose logs --tail 50 planka >&2
exit 1
