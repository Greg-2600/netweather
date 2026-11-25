# Soak run

A 72-hour run at the default fleet, with no restart, no unbounded growth, and a
stable probe rate. This is the last check before calling a build "ready to leave
unattended", and it is an operator step rather than a CI job: a three-day run
cannot live in a pull-request gate.

## Recipe

1. Start a fresh instance with a real target list:

   ```sh
   NETWEATHER_CONFIG=/config/targets.yaml \
   NETWEATHER_LOG_LEVEL=info \
   NETWEATHER_DAILY_MB=2048 \
   docker compose up -d
   ```

   Use `NETWEATHER_SEED=none` if the file is the whole board, so the shipped
   full fleet does not run beside it.

2. Record the starting state:

   ```sh
   docker compose exec netweather /netweather backup -o /data/soak-start.db
   curl -s -H "Authorization: Bearer $TOKEN" http://localhost:2600/api/v1/meta
   docker compose exec netweather /netweather debug bundle -o /data/soak-start.tar.gz
   ```

3. Leave it running for 72 hours. Do not restart it, and do not change the
   config.

4. Record the ending state the same way, then stop:

   ```sh
   docker compose exec netweather /netweather backup -o /data/soak-end.db
   curl -s -H "Authorization: Bearer $TOKEN" http://localhost:2600/api/v1/meta
   docker compose exec netweather /netweather debug bundle -o /data/soak-end.tar.gz
   docker compose down
   ```

## What to record

- database size at start and end (the `.db` file, not the WAL)
- `dropped` and `write_errors` from `/api/v1/meta` (both should be zero)
- `targets` count and `unmeasurable` count
- `probe_rate_per_second` from `/metrics`, at the start, middle, and end
- the number of open incidents at each point
- whether any target flapped more than six times in 24 hours (`internal/store`
  counts `state_change` rows per target per day; a flapping target is a
  low-confidence probe, not a bug, but a *new* one on a previously stable target
  is worth investigating)
- any `event=log_dropped`, `event=selfcheck_failed`, or `event=vantage_changed`
  in `docker compose logs`

## Pass criteria

- The process does not restart and exits cleanly on `docker compose down`.
- Database growth is within the `SCHEMA.md` §16 estimate (roughly 640 MB steady
  state at 1000 targets; a few hundred MB at most for a real fleet).
- `dropped` and `write_errors` stay at zero.
- No state flapping beyond the classifier's own low-confidence rule.
- The probe rate is stable to within 10% across the run.

## What the code already proves

The code-side readiness is covered by the test suite, which is what makes the
72-hour run a confirmation rather than a discovery:

- `devtool verify` is green (format, vet, lint, tests, coverage gates, build).
- The compose suite ran the release image for ~110 s with no restart and no
  unbounded growth.
- The real-endpoint suite ran six public targets through the real instrument and
  classifier in 1.58 s.
- Three consecutive full-suite runs with `-shuffle=on` and `-count=1` passed
  with zero failures across api, web, probe, store, and e2e.
- The event tracker's baseline is built before the server serves, so a change
  before the first snapshot is not lost.
- The pre-migration backup and the restore drill are tested, so an upgrade
  during a long run does not lose the database.

A failure against any of the pass criteria above is a bug to file, not a
configuration problem: the suite exists to make the 72-hour run boring.
