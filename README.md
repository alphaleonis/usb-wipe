# usbwipe

Terminal UI for wiping and reformatting USB drives on Linux.

Built with [Bubble Tea](https://github.com/charmbracelet/bubbletea).

## Features

- Detect and list removable USB drives
- Browse partition contents (read-only) before wiping
- Wipe modes:
  - **Wipe** — reformat only (fastest)
  - **Wipe + Quick Verify** — bad sector check during format (`mkfs -c` for FAT32, `badblocks` read-only for exFAT)
  - **Wipe + Full Verify** — destructive surface scan with `badblocks -w` before reformatting (slow, thorough)
- Filesystem choices: FAT32 or exFAT
- Eject after wipe

## Safety

- Only targets devices that are both `removable` and on a USB bus (verified via sysfs)
- Refuses to wipe anything mounted at critical system paths (`/`, `/boot`, `/home`, `/usr`, `/var`, `/etc`, `/srv`, `/opt`, `/tmp`)
- Requires explicit "yes" confirmation before wiping
- File browsing mounts partitions read-only

## Requirements

- Linux
- Root privileges
- `wipefs`, `sfdisk`, `mkfs.vfat`, `blkid`, `eject` (typically part of `util-linux` and `dosfstools`)
- `mkfs.exfat` (from `exfatprogs`) — for exFAT formatting
- `badblocks` (from `e2fsprogs`) — for verify modes

## Install

```
go build -o usbwipe .
sudo cp usbwipe /usr/local/bin/
```

## Usage

```
sudo usbwipe [-v]
```

`-v` enables verbose diagnostic output to stderr.

## Navigation

| View | Keys |
|------|------|
| Device list | `↑/↓` navigate, `enter` select, `r` refresh, `q/esc` quit |
| Device detail | `↑/↓` navigate partitions, `enter` browse, `w` wipe, `esc` back |
| File browser | `↑/↓` navigate, `enter` open dir, `backspace` parent/exit, `esc` exit |
| Wipe mode | `↑/↓` navigate, `enter` select, `esc` cancel |
| Filesystem | `↑/↓` navigate, `enter` select, `esc` back |
| Volume label | type label, `enter` continue, `esc` back |
| Confirm | type "yes", `enter` confirm, `esc` back |
| Wipe done | `e` eject & quit, `enter/q` quit, `esc` back to list |
