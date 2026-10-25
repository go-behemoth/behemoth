#!/bin/sh
# Entrypoint of the migrate image (see Dockerfile). It writes the next
# migration with the example's behemoth command, then applies the script it
# wrote with psql, the way the example applies its migrations by hand.
#
# When the database already matches, generate writes no script and nothing
# is applied, so the job runs on every "docker compose up". The database is
# what generate compares with, so a script from an earlier run is never
# applied twice.
set -eu

before=$(ls migrations/*.sql 2>/dev/null || true)

behemoth generate -confirm

for script in migrations/*.sql; do
	[ -e "$script" ] || continue
	if printf '%s\n' "$before" | grep -qxF "$script"; then
		continue
	fi
	echo "Applying $script"
	# One transaction: PostgreSQL rolls back the whole script if a statement
	# fails, and the next run generates it again.
	psql "$DATABASE_URL" --quiet -v ON_ERROR_STOP=1 --single-transaction -f "$script"
done
	