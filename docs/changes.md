# changes

Read when: consuming the local message stream from a durable sequence cursor.

`wacli changes` exposes insert, edit, revoke, and delete mutations from the selected local store. It reads `wacli.db` without taking the store lock, so it can run alongside `sync --follow`. Use `--json` for the stable envelope consumed by scripts.

## Commands

```bash
wacli changes list --json --after-seq N [--limit N]
wacli changes status --json [--lookback-s N]
```

## Cursor contract

- A cursor is the pair `store_instance_id` and `seq`; message timestamps are not resume cursors.
- `changes list` returns rows with `seq > --after-seq` in ascending sequence order. The default page size is 200 and the maximum is 500.
- Each change includes `kind`, `origin`, message identity metadata, and the current stored message. `message` is `null` and the page's `purged` count increases if message retention already removed that row.
- `origin` is `live` or `history`. Consumers that only want new inbound delivery should filter for live insert rows that are not `from_me`.
- A cursor older than retained change history fails with `cursor_gap`. A cursor beyond the store's allocated sequence fails with `cursor_future`; neither condition falls back to timestamps.
- `changes status` reports the current store identity and `bootstrap_seq`. The bootstrap sequence maps the requested wall-clock lookback to a sequence once; subsequent reads should resume only by sequence.

Both commands honor `--read-only` and `WACLI_READONLY=1`.

The sync daemon performs store migrations during a writable open. After upgrading wacli, run the sync daemon once before polling `changes`; a read-only poll never auto-migrates an older store and returns `store_not_migrated` until that writable open completes.

## Examples

```bash
wacli changes status --json --lookback-s 3600
wacli changes list --json --after-seq 4900 --limit 200
```
