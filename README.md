# DiskScope

Fast disk usage analyzer with a web UI, written in Go. Single binary, no dependencies, Windows and Linux.

- Pick a drive (C:, D:, mount points) or any folder
- Parallel scan with live progress; does not follow symlinks or junctions
- Treemap + sorted list, drill down into folders
- Largest files, heaviest folders, usage by file type
- Running programs whose executable lives in the folder you are viewing (PID, RAM)

## Build

```sh
go build -trimpath -ldflags "-s -w" -o diskscope.exe .
GOOS=linux CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o diskscope-linux .
```

Requires Go 1.23+.

## Run

```sh
./diskscope            # opens http://127.0.0.1:8765
./diskscope -port 9000 -no-browser
```

Run as administrator / root to read every folder.
