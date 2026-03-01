## Project

usbwipe — Terminal UI for wiping USB drives on Linux. Single-file Go program using Bubble Tea.

## Structure

- `main.go` — entire application. TUI model/update/view logic at the top; backend functions (detection, wipe, exec) at the bottom.
- `go.mod` / `go.sum` — Go 1.25, direct deps: bubbletea, bubbles, lipgloss.

## Branches

- `bubbletea-tui` — current, active development (Bubble Tea TUI)
- `huh-wizard` — previous version using huh (linear wizard flow), preserved for reference
- `main` — empty initial commit

## Architecture

Flat state machine with 9 view states:

```
viewDeviceList → viewDeviceDetail → viewFileBrowser
                                  → viewWipeMode → viewWipeFS → viewWipeLabel → viewWipeConfirm → viewWiping → viewWipeDone
```

Each view has an `update*` method (handles tea.Msg) and a `render*` method (returns string). The top-level `Update()` and `View()` dispatch by `m.view`.

Async operations use `tea.Cmd` functions that return typed messages (e.g. `wipeCmd` → `wipeResultMsg`). The view must be set to the receiving view **before** returning the command, or the response message will be dispatched to the wrong handler.

## Key patterns

- **Value receivers everywhere** except `buildPartTable`, `startBrowse`, and `cleanupBrowse` (pointer receivers because they mutate model fields used by the returned `tea.Cmd`).
- **File browser mount/unmount**: if a partition is already mounted, browse in place (`browseMntPath = ""`). If not, mount read-only to `/tmp/usbwipe-{name}` and set `browseMntPath` so cleanup knows to unmount.
- **Partition table in detail view**: built fresh via `buildPartTable()` each time a device is selected (not reused/mutated).

## Backend functions

`detectUSBDevices`, `readSysfs`, `humanSize`, `parseProcMounts`, `parseBlkid`, `checkMountSafety`, `doWipe`, `runCmd`, `runCmdStdin` — these are pure logic, no TUI dependency. Detection requires `/sys/block` and checks both `removable` flag and USB bus path via sysfs symlinks.

## External tools used by doWipe

- `umount`, `wipefs`, `sfdisk` — always
- `mkfs.vfat` — FAT32
- `mkfs.exfat` — exFAT
- `badblocks -w` — full verify mode (destructive write test)
- `badblocks -s` — quick verify for exFAT (since mkfs.exfat has no `-c` flag)
- `mkfs.vfat -c` — quick verify for FAT32

## Build & test

```sh
go build -o usbwipe .
go mod tidy          # after changing deps
./usbwipe          # must fail with "must be run as root"
sudo ./usbwipe     # runs the TUI
sudo ./usbwipe -v  # verbose to stderr
```

No test suite. Manual testing only — requires physical USB drives and root.

## Common pitfalls

- When adding a new async flow, always set `m.view` to the target view **before** returning the `tea.Cmd`. Otherwise the response message routes to the wrong update handler.
- The `table.Model` from bubbles doesn't reliably update when mutated after construction. Build a fresh table instead of calling `SetRows`/`SetHeight` on an empty one.
- Pointer-receiver methods called from value-receiver methods work (Go takes `&m` of the local copy), but only because the modified `m` is what gets returned.

## Code style

- Sections delimited with `// ── Section Name ──` banner comments.
- Each view state has a paired `update*` / `render*` method.
- Async commands are standalone functions (e.g. `detectCmd`, `wipeCmd`); inline closures only when capturing model state (e.g. `startBrowse`).
