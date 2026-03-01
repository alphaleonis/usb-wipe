package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/huh/spinner"
)

var verbose bool

// usbPathRe matches sysfs USB bus segments like /usb1/, /usb4/, etc.
var usbPathRe = regexp.MustCompile(`/usb\d+/`)

type Partition struct {
	Name, Path, MountPoint, FSType, Label string
}

type USBDevice struct {
	Name, Path, SizeHuman, Model, Vendor string
	FSType, Label, MountPoint            string // whole-device filesystem (superfloppy)
	SizeSectors                          int64
	Partitions                           []Partition
}

func main() {
	flag.BoolVar(&verbose, "v", false, "verbose diagnostic output")
	flag.Parse()

	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "Error: must be run as root")
		os.Exit(1)
	}

	devices, err := detectUSBDevices()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error detecting USB devices: %v\n", err)
		os.Exit(1)
	}
	if len(devices) == 0 {
		fmt.Println("No removable USB drives found.")
		os.Exit(0)
	}

	dev, err := promptSelectDevice(devices)
	if err != nil {
		if err == huh.ErrUserAborted {
			fmt.Println("Aborted.")
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	printDeviceDetails(dev)

	label, err := promptVolumeLabel()
	if err != nil {
		if err == huh.ErrUserAborted {
			fmt.Println("Aborted.")
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if err := checkMountSafety(dev); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	confirmed, err := promptConfirmWipe(dev)
	if err != nil {
		if err == huh.ErrUserAborted {
			fmt.Println("Aborted.")
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if !confirmed {
		fmt.Println("Aborted.")
		os.Exit(0)
	}

	var wipeErr error
	err = spinner.New().
		Title(fmt.Sprintf("Wiping %s...", dev.Path)).
		Action(func() { wipeErr = doWipe(dev, label) }).
		Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if wipeErr != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", wipeErr)
		os.Exit(1)
	}

	shouldEject, err := promptEject(dev)
	if err != nil {
		if err == huh.ErrUserAborted {
			fmt.Println("Aborted.")
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if shouldEject {
		if out, ejErr := runCmd("eject", dev.Path); ejErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: eject failed: %v\n%s\n", ejErr, out)
		} else {
			fmt.Printf("Ejected %s.\n", dev.Path)
		}
	}

	fmt.Println("Done.")
}

func logv(format string, args ...any) {
	if verbose {
		fmt.Fprintf(os.Stderr, "[verbose] "+format+"\n", args...)
	}
}

func detectUSBDevices() ([]USBDevice, error) {
	entries, err := os.ReadDir("/sys/block")
	if err != nil {
		return nil, err
	}

	mounts := parseProcMounts()
	blkidInfo := parseBlkid()

	var devices []USBDevice
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "sd") {
			continue
		}

		base := "/sys/block/" + name
		logv("examining %s", name)

		// Safety gate 1: must be removable
		removable := readSysfs(base + "/removable")
		logv("  removable = %q", removable)
		if removable != "1" {
			logv("  skipped: not removable")
			continue
		}

		// Safety gate 2: sysfs device path must be on a USB bus
		deviceLink := base + "/device"
		resolved, err := filepath.EvalSymlinks(deviceLink)
		if err != nil {
			logv("  skipped: cannot resolve %s: %v", deviceLink, err)
			continue
		}
		logv("  device path = %s", resolved)
		if !usbPathRe.MatchString(resolved) {
			logv("  skipped: device path does not contain USB bus segment")
			continue
		}

		// Skip zero-size devices
		sizeStr := readSysfs(base + "/size")
		var sizeSectors int64
		fmt.Sscanf(sizeStr, "%d", &sizeSectors)
		if sizeSectors == 0 {
			logv("  skipped: zero size")
			continue
		}

		sizeBytes := sizeSectors * 512
		sizeHuman := humanSize(sizeBytes)

		model := strings.TrimSpace(readSysfs(base + "/device/model"))
		vendor := strings.TrimSpace(readSysfs(base + "/device/vendor"))
		logv("  accepted: %s %s, %s", vendor, model, sizeHuman)

		dev := USBDevice{
			Name:        name,
			Path:        "/dev/" + name,
			SizeSectors: sizeSectors,
			SizeHuman:   sizeHuman,
			Model:       model,
			Vendor:      vendor,
		}

		// Detect partitions
		partGlob, _ := filepath.Glob(base + "/" + name + "*")
		for _, pp := range partGlob {
			pName := filepath.Base(pp)
			pPath := "/dev/" + pName
			part := Partition{
				Name: pName,
				Path: pPath,
			}
			if mp, ok := mounts[pPath]; ok {
				part.MountPoint = mp
			}
			if info, ok := blkidInfo[pPath]; ok {
				part.FSType = info.fsType
				part.Label = info.label
			}
			dev.Partitions = append(dev.Partitions, part)
		}

		// Check whole-device filesystem (superfloppy layout)
		if info, ok := blkidInfo[dev.Path]; ok {
			dev.FSType = info.fsType
			dev.Label = info.label
		}
		if mp, ok := mounts[dev.Path]; ok {
			dev.MountPoint = mp
		}

		devices = append(devices, dev)
	}

	return devices, nil
}

func readSysfs(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func humanSize(bytes int64) string {
	const (
		KB = 1000
		MB = 1000 * KB
		GB = 1000 * MB
		TB = 1000 * GB
	)
	switch {
	case bytes >= TB:
		return fmt.Sprintf("%.1f TB", float64(bytes)/float64(TB))
	case bytes >= GB:
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(GB))
	case bytes >= MB:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(MB))
	default:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(KB))
	}
}

func parseProcMounts() map[string]string {
	mounts := make(map[string]string)
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return mounts
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 {
			mounts[fields[0]] = fields[1]
		}
	}
	return mounts
}

type blkidEntry struct {
	fsType, label string
}

func parseBlkid() map[string]blkidEntry {
	info := make(map[string]blkidEntry)
	out, err := exec.Command("blkid", "-o", "export").Output()
	if err != nil {
		return info
	}

	var devname, fstype, label string
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if devname != "" {
				info[devname] = blkidEntry{fsType: fstype, label: label}
			}
			devname, fstype, label = "", "", ""
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "DEVNAME":
			devname = v
		case "TYPE":
			fstype = v
		case "LABEL":
			label = v
		}
	}
	if devname != "" {
		info[devname] = blkidEntry{fsType: fstype, label: label}
	}
	return info
}

func promptSelectDevice(devices []USBDevice) (USBDevice, error) {
	options := make([]huh.Option[int], len(devices))
	for i, d := range devices {
		desc := fmt.Sprintf("%s  %s  %s %s", d.Path, d.SizeHuman, d.Vendor, d.Model)
		options[i] = huh.NewOption(desc, i)
	}

	var idx int
	sel := huh.NewSelect[int]().
		Title("Select a USB drive to wipe").
		Options(options...).
		Value(&idx)

	if err := huh.Run(sel); err != nil {
		return USBDevice{}, err
	}
	return devices[idx], nil
}

func printDeviceDetails(dev USBDevice) {
	fmt.Println()
	fmt.Printf("  Device:  %s\n", dev.Path)
	fmt.Printf("  Model:   %s %s\n", dev.Vendor, dev.Model)
	fmt.Printf("  Size:    %s\n", dev.SizeHuman)
	if len(dev.Partitions) > 0 {
		fmt.Println("  Partitions:")
		for _, p := range dev.Partitions {
			line := fmt.Sprintf("    %s", p.Path)
			if p.FSType != "" {
				line += fmt.Sprintf("  [%s]", p.FSType)
			}
			if p.Label != "" {
				line += fmt.Sprintf("  \"%s\"", p.Label)
			}
			if p.MountPoint != "" {
				line += fmt.Sprintf("  mounted at %s", p.MountPoint)
			}
			fmt.Println(line)
		}
	} else if dev.FSType != "" {
		line := fmt.Sprintf("  FS:      [%s]", dev.FSType)
		if dev.Label != "" {
			line += fmt.Sprintf("  \"%s\"", dev.Label)
		}
		if dev.MountPoint != "" {
			line += fmt.Sprintf("  mounted at %s", dev.MountPoint)
		}
		line += "  (no partition table)"
		fmt.Println(line)
	} else {
		fmt.Println("  Partitions: none")
	}
	fmt.Println()
}

func promptVolumeLabel() (string, error) {
	label := "USB"
	inp := huh.NewInput().
		Title("Volume label (max 11 chars)").
		Value(&label).
		Validate(func(s string) error {
			if len(s) == 0 {
				return fmt.Errorf("label cannot be empty")
			}
			if len(s) > 11 {
				return fmt.Errorf("label must be 11 characters or fewer")
			}
			return nil
		})

	if err := huh.Run(inp); err != nil {
		return "", err
	}
	return strings.ToUpper(label), nil
}

func checkMountSafety(dev USBDevice) error {
	critical := map[string]bool{"/": true, "/boot": true, "/home": true}
	if critical[dev.MountPoint] {
		return fmt.Errorf("refusing to wipe: %s is mounted at %s", dev.Path, dev.MountPoint)
	}
	for _, p := range dev.Partitions {
		if critical[p.MountPoint] {
			return fmt.Errorf("refusing to wipe: %s is mounted at %s", p.Path, p.MountPoint)
		}
	}
	return nil
}

func promptConfirmWipe(dev USBDevice) (bool, error) {
	desc := fmt.Sprintf("%s %s", dev.Vendor, dev.Model)
	msg := fmt.Sprintf("WIPE ALL DATA on %s (%s, %s)?", dev.Path, strings.TrimSpace(desc), dev.SizeHuman)

	var confirmed bool
	conf := huh.NewConfirm().
		Title(msg).
		Affirmative("Yes, wipe it").
		Negative("No").
		Value(&confirmed)

	err := huh.Run(conf)
	return confirmed, err
}

func doWipe(dev USBDevice, label string) error {
	// Unmount whole-device mount (superfloppy layout)
	if dev.MountPoint != "" {
		if out, err := runCmd("umount", dev.Path); err != nil {
			return fmt.Errorf("umount %s: %w\n%s", dev.Path, err, out)
		}
	}

	// Unmount all mounted partitions
	for _, p := range dev.Partitions {
		if p.MountPoint != "" {
			if out, err := runCmd("umount", p.Path); err != nil {
				return fmt.Errorf("umount %s: %w\n%s", p.Path, err, out)
			}
		}
	}

	// Wipe filesystem signatures
	if out, err := runCmd("wipefs", "-a", dev.Path); err != nil {
		return fmt.Errorf("wipefs: %w\n%s", err, out)
	}

	// Create MBR partition table with single FAT32 LBA partition
	sfdiskInput := "label: dos\ntype=c\n"
	if out, err := runCmdStdin("sfdisk", sfdiskInput, "--lock", dev.Path); err != nil {
		return fmt.Errorf("sfdisk: %w\n%s", err, out)
	}

	// Format as FAT32
	part1 := dev.Path + "1"
	if out, err := runCmd("mkfs.vfat", "-F", "32", "-n", label, part1); err != nil {
		return fmt.Errorf("mkfs.vfat: %w\n%s", err, out)
	}

	return nil
}

func promptEject(dev USBDevice) (bool, error) {
	var eject bool
	conf := huh.NewConfirm().
		Title(fmt.Sprintf("Eject %s?", dev.Path)).
		Affirmative("Yes").
		Negative("No").
		Value(&eject)

	err := huh.Run(conf)
	return eject, err
}

func runCmd(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func runCmdStdin(name string, stdin string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
