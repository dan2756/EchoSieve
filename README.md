# dedupe-stream

Passes only never-before-seen lines, remembered across runs.

`dedupe-stream` reads lines on stdin and prints each one only the first time it
has *ever* seen it: not just earlier in this run, but in every previous run
too. Point a `tail -f` at it and you get a feed of genuinely new log lines.

## Install

```sh
go install github.com/dan2756/dedupe-stream@latest
```

## Usage

```sh
tail -f app.log | dedupe-stream --name app-errors
```

| Flag | Meaning |
| --- | --- |
| `--name NAME` | state to use (default `default`) |
| `--file PATH` | use the state file at `PATH` instead of a named state |
| `--ttl DURATION` | forget lines first seen longer ago than this (`12h`, `7d`, `2w`, `1d12h`) |
| `--exact` | store whole keys instead of 64-bit hashes: no collisions, more space |
| `--key REGEX` | compare only the first capture group of `REGEX` (or the whole match) |
| `-f`, `--field N` | compare only field `N`, counting from 1 |
| `-d`, `--delimiter SEP` | field separator for `--field` (default: runs of whitespace) |
| `-i`, `--ignore-case` | ignore case differences |
| `--trim` | ignore leading and trailing whitespace |
| `--strip-numbers` | treat every run of digits as equal (`retry 3` = `retry 7`) |
| `--dry-run` | show what would pass, but record nothing |
| `--invert` | print only lines that *have* been seen before |
| `--reset` | delete the state for `--name` / `--file` |
| `--stats` | show entry count, file size and oldest entry |
| `--list` | list the named states |
| `-h`, `--help` | show help |
| `--version` | print the version |

Exit status is 0 on success, 1 on errors (unreadable input, a corrupt or
foreign state file) and 2 for invalid usage.

Output is written one line at a time, so it works in the middle of a pipe.
Each new line is printed first and then recorded, so a crash can at worst show
one line again; it never silently swallows one.

State for `--name NAME` lives in `$XDG_STATE_HOME/dedupe-stream/NAME.seen`
(default `~/.local/state/dedupe-stream/NAME.seen`). Each seen line costs 16
bytes on disk, an 8-byte hash and the time it was first seen, and is loaded
into memory on startup.

## Choosing a key

By default two lines are "the same" only if they are byte-for-byte identical
(apart from a trailing `\r`). Log lines usually carry timestamps, request IDs
or counters that make every line unique, so you will usually want to narrow
the comparison:

```sh
# one line per distinct error message, ignoring the timestamp prefix
tail -f app.log | dedupe-stream --name app-errors --key 'ERROR (.*)'

# one line per client IP in an access log
tail -f access.log | dedupe-stream --name new-clients -f 1

# new usernames from a CSV export, whatever the case
dedupe-stream --name users -f 2 -d , -i < export.csv

# "retry 3 of 5" and "retry 4 of 5" count as the same line
journalctl -f -o cat | dedupe-stream --name journal --strip-numbers --trim
```

The key is built in this order: `--key` or `--field` picks part of the line,
then `--trim`, `--ignore-case` and `--strip-numbers` normalize it. A line that
`--key` does not match, or that has fewer than `N` fields, is compared whole.

`--dry-run` and `--invert` combine: `--invert --dry-run` shows the lines a
normal run would drop, without changing anything. On its own, `--invert`
updates the state exactly like a normal run; it just prints the other half.

## Forgetting old lines

With `--ttl`, a line is forgotten once it was first seen longer ago than the
TTL, and passes again the next time it shows up. That turns "never seen
before" into "not seen this week", which is handy for alerts that should
repeat now and then:

```sh
tail -f app.log | dedupe-stream --name app-errors --key 'ERROR (.*)' --ttl 7d
```

The TTL counts from when a line was *recorded*, not from when it was last
seen, so a line that keeps recurring passes again once per TTL. When a run
with `--ttl` starts and the state file holds expired entries, the file is
compacted (rewritten without them) before any input is read. Without `--ttl`
nothing ever expires, whatever earlier runs used.

## Hashes, collisions and `--exact`

By default each key is stored as the first 8 bytes of its SHA-256. That keeps
records small and fixed-size, but two different lines could in principle get
the same hash, and then the second would be wrongly dropped. With `n`
distinct lines stored, the chance that *any* pair collides is about
`n² / 2⁶⁵`:

| lines stored | chance of any collision |
| --- | --- |
| 100,000 | about 1 in 3.7 billion |
| 1,000,000 | about 1 in 37 million |
| 100,000,000 | about 1 in 3,700 |

If that is still too much, `--exact` stores every key verbatim (12 bytes of
overhead plus the key) and compares keys byte for byte. It costs more disk
and memory, especially for long lines, so it pairs well with `--key` or
`--field`. The record format is chosen when a state file is created: an
`--exact` state stays exact without repeating the flag, and `--exact` on an
existing hash state is refused (pick another `--name` or `--reset` first).

## Managing state

```console
$ dedupe-stream --list
NAME        ENTRIES  SIZE      NEWEST
app-errors  1532     24.0 KiB  2025-09-13 10:58
default     12       208 B     2025-09-12 08:15

$ dedupe-stream --name app-errors --stats --ttl 7d
name     app-errors
file     /home/dj/.local/state/dedupe-stream/app-errors.seen
format   sha256/64 hashes
entries  1501
expired  31 (ttl 7d)
size     24.0 KiB
oldest   2025-09-06 21:02:11 UTC (6d ago)
newest   2025-09-13 10:58:40 UTC (just now)

$ dedupe-stream --name app-errors --reset
dedupe-stream: removed /home/dj/.local/state/dedupe-stream/app-errors.seen
```

`--reset` refuses to delete a file that is not a dedupe-stream state file, so
a mistyped `--file app.log --reset` is harmless.

## Running several at once

Any number of processes can use the same state at the same time, for example
one `tail -f` per log file all feeding one `--name`. Each takes an exclusive
`flock` on the state file only while it records a new line, after first
reading whatever the others have appended, so a line is printed by exactly
one of them. Lines already known are answered from memory without touching
the file. Compaction and `--reset` replace the file under the same lock, and
running processes switch to the new file the next time they record a line.

## License

MIT
