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
| `-h`, `--help` | show help |
| `--version` | print the version |

Output is written one line at a time, so it works in the middle of a pipe.
Each new line is printed first and then recorded, so a crash can at worst show
one line again; it never silently swallows one.

State for `--name NAME` lives in `$XDG_STATE_HOME/dedupe-stream/NAME.seen`
(default `~/.local/state/dedupe-stream/NAME.seen`). Each seen line costs 16
bytes on disk: an 8-byte hash and the time it was first seen.

## License

MIT
