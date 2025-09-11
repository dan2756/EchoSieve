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
| `--key REGEX` | compare only the first capture group of `REGEX` (or the whole match) |
| `-f`, `--field N` | compare only field `N`, counting from 1 |
| `-d`, `--delimiter SEP` | field separator for `--field` (default: runs of whitespace) |
| `-i`, `--ignore-case` | ignore case differences |
| `--trim` | ignore leading and trailing whitespace |
| `--strip-numbers` | treat every run of digits as equal (`retry 3` = `retry 7`) |
| `--dry-run` | show what would pass, but record nothing |
| `--invert` | print only lines that *have* been seen before |
| `-h`, `--help` | show help |
| `--version` | print the version |

Output is written one line at a time, so it works in the middle of a pipe.
Each new line is printed first and then recorded, so a crash can at worst show
one line again; it never silently swallows one.

State for `--name NAME` lives in `$XDG_STATE_HOME/dedupe-stream/NAME.seen`
(default `~/.local/state/dedupe-stream/NAME.seen`). Each seen line costs 16
bytes on disk: an 8-byte hash and the time it was first seen.

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

## License

MIT
