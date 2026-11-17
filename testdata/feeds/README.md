# Recorded feed shapes

One representative payload per external adapter, used by
`internal/feeds/contract_test.go`.

They are **shapes, not live calls**: CI parses these files and never contacts a
third party (QUALITY.md §5.6). Each file contains the fields netweather actually
depends on plus a little surrounding noise, because the parser is deliberately
tolerant of unknown keys.

| File | Adapter |
|---|---|
| `ioda.json` | `feeds.IODA`, `/v2/outages/events` |
| `radar.json` | `feeds.Radar`, `/annotations/outages` |
| `statuspage.json` | `feeds.Statuspage`, `/api/v2/summary.json` |
| `gcp-status.json` | `feeds.GCPStatus`, Google Cloud incidents JSON |
| `aws-status.xml` | `feeds.RSSStatus` as `aws_status` |
| `azure-status.xml` | `feeds.RSSStatus` as `azure_status` |

## Refreshing a fixture

1. Capture one real response into the matching file. Keep it small; the
   parsers cap stored payloads at 8 KiB anyway.
2. Run `go test ./internal/feeds/ -run TestFeedContract`.
3. If the upstream moved a field, the contract test fails on the field name,
   which is the point: a silent shape change becomes a red test, not an empty
   feed on a real instance.

Do not add live calls to the test. A feed that is down must not fail the build;
it must show as `stale` on the board.
