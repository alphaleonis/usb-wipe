package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var verbose bool

var usbPathRe = regexp.MustCompile(`/usb\d+/`)

// ── Types ────────────────────────────────────────────────────────────────────

type Partition struct {
	Name, Path, MountPoint, FSType, Label string
}

type USBDevice struct {
	Name, Path, SizeHuman, Model, Vendor string
	FSType, Label, MountPoint            string // whole-device filesystem (superfloppy)
	SizeSectors                          int64
	Partitions                           []Partition
}

type viewState int

const (
	viewDeviceList viewState = iota
	viewDeviceDetail
	viewFileBrowser
	viewWipeMode
	viewWipeFS
	viewWipeLabel
	viewWipeConfirm
	viewWiping
	viewWipeDone
)

type wipeMode int

const (
	wipeFast wipeMode = iota
	wipeQuickVerify
	wipeFullVerify
)

var wipeModeLabels = []string{
	"Wipe",
	"Wipe + Quick Verify",
	"Wipe + Full Verify",
}

var wipeModeDescs = []string{
	"Reformat only (fastest)",
	"Reformat with bad sector check (mkfs -c)",
	"Full surface scan with badblocks, then reformat (slow)",
}

type fsType int

const (
	fsFAT32 fsType = iota
	fsExFAT
)

var fsTypeLabels = []string{
	"FAT32",
	"exFAT",
}

var fsTypeDescs = []string{
	"Universal compatibility, 4 GB file size limit",
	"Large file support, modern OS compatibility",
}

// ── Messages ─────────────────────────────────────────────────────────────────

type devicesRefreshedMsg struct {
	devices []USBDevice
	err     error
}

type mountResultMsg struct {
	path string // mount point path
	err  error
}

type dirListingMsg struct {
	entries []fs.DirEntry
	err     error
}

type unmountDoneMsg struct{ err error }

type wipeResultMsg struct{ err error }

type ejectResultMsg struct{ err error }

// ── File browser entry ───────────────────────────────────────────────────────

type fileEntry struct {
	name    string
	size    int64
	mode    fs.FileMode
	isDir   bool
	modTime string
}

// ── Model ────────────────────────────────────────────────────────────────────

type model struct {
	view    viewState
	width   int
	height  int
	err     string
	devices []USBDevice

	// Device list
	table table.Model

	// Device detail
	selectedDev int
	partTable   table.Model // partition list (reused table component)

	// File browser
	browseDir     string // current directory being listed
	browseMntPath string // non-empty if we mounted it ourselves (unmount on exit)
	browseEntries []fileEntry
	browseCursor  int
	browsePartIdx int // -1 for whole-device, 0..N for partition index

	// Wipe flow
	wipeMode       wipeMode
	wipeModeCursor int
	fsType         fsType
	fsCursor       int
	labelInput     textinput.Model
	confirmInput   textinput.Model
	spinner        spinner.Model
	wipeErr        error

	// Eject
	ejectErr error
}

// ── Styles ───────────────────────────────────────────────────────────────────

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("12"))

	errStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("9")).
			Bold(true)

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8"))

	selectedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("14")).
			Bold(true)

	dimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8"))

	successStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("10")).
			Bold(true)
)

// ── Constructor ──────────────────────────────────────────────────────────────

func newModel(devices []USBDevice) model {
	// Table
	columns := []table.Column{
		{Title: "Path", Width: 12},
		{Title: "Size", Width: 10},
		{Title: "Vendor", Width: 12},
		{Title: "Model", Width: 20},
	}
	rows := devicesToRows(devices)
	t := table.New(
		table.WithColumns(columns),
		table.WithRows(rows),
		table.WithFocused(true),
		table.WithHeight(len(rows)+1),
	)
	s := table.DefaultStyles()
	s.Header = s.Header.
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("240")).
		BorderBottom(true).
		Bold(true)
	s.Selected = s.Selected.
		Foreground(lipgloss.Color("229")).
		Background(lipgloss.Color("57")).
		Bold(false)
	t.SetStyles(s)

	// Label input
	li := textinput.New()
	li.Placeholder = "USB"
	li.SetValue("USB")
	li.CharLimit = 11
	li.Width = 20

	// Confirm input
	ci := textinput.New()
	ci.Placeholder = "type yes to confirm"
	ci.CharLimit = 10
	ci.Width = 20

	// Spinner
	sp := spinner.New()
	sp.Spinner = spinner.Dot

	return model{
		view:    viewDeviceList,
		devices: devices,
		table: t,

		browsePartIdx: -1,

		labelInput:   li,
		confirmInput: ci,
		spinner:      sp,
	}
}

func devicesToRows(devices []USBDevice) []table.Row {
	rows := make([]table.Row, len(devices))
	for i, d := range devices {
		rows[i] = table.Row{d.Path, d.SizeHuman, d.Vendor, d.Model}
	}
	return rows
}

// ── Init ─────────────────────────────────────────────────────────────────────

func (m model) Init() tea.Cmd {
	return nil
}

// ── Update ───────────────────────────────────────────────────────────────────

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	}

	switch m.view {
	case viewDeviceList:
		return m.updateDeviceList(msg)
	case viewDeviceDetail:
		return m.updateDeviceDetail(msg)
	case viewFileBrowser:
		return m.updateFileBrowser(msg)
	case viewWipeMode:
		return m.updateWipeMode(msg)
	case viewWipeFS:
		return m.updateWipeFS(msg)
	case viewWipeLabel:
		return m.updateWipeLabel(msg)
	case viewWipeConfirm:
		return m.updateWipeConfirm(msg)
	case viewWiping:
		return m.updateWiping(msg)
	case viewWipeDone:
		return m.updateWipeDone(msg)
	}
	return m, nil
}

// ── Device List ──────────────────────────────────────────────────────────────

func (m model) updateDeviceList(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "r":
			return m, detectCmd
		case "enter":
			idx := m.table.Cursor()
			if idx >= 0 && idx < len(m.devices) {
				m.selectedDev = idx
				m.buildPartTable()
				m.view = viewDeviceDetail
			}
			return m, nil
		}
	case devicesRefreshedMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, nil
		}
		m.err = ""
		m.devices = msg.devices
		rows := devicesToRows(m.devices)
		m.table.SetRows(rows)
		m.table.SetHeight(len(rows) + 1)
		return m, nil
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m model) renderDeviceList() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("USB Wipe — Device List"))
	b.WriteString("\n\n")
	if len(m.devices) == 0 {
		b.WriteString("  No USB devices found.\n")
	} else {
		b.WriteString(m.table.View())
		b.WriteString("\n")
	}
	if m.err != "" {
		b.WriteString(errStyle.Render("  Error: "+m.err) + "\n")
	}
	b.WriteString("\n")
	b.WriteString(helpStyle.Render("  ↑/↓ navigate • enter select • r refresh • q/esc quit"))
	return b.String()
}

// ── Device Detail ────────────────────────────────────────────────────────────

func (m *model) buildPartTable() {
	dev := m.devices[m.selectedDev]
	var rows []table.Row
	if len(dev.Partitions) > 0 {
		for _, p := range dev.Partitions {
			rows = append(rows, table.Row{p.Path, p.FSType, p.Label, p.MountPoint})
		}
	} else if dev.FSType != "" {
		// Superfloppy: show whole device as the single browseable entry
		rows = append(rows, table.Row{dev.Path, dev.FSType, dev.Label, dev.MountPoint})
	}
	cols := []table.Column{
		{Title: "Partition", Width: 14},
		{Title: "FS", Width: 8},
		{Title: "Label", Width: 14},
		{Title: "Mount", Width: 24},
	}
	h := len(rows) + 1
	if h < 2 {
		h = 2
	}
	s := table.DefaultStyles()
	s.Header = s.Header.
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("240")).
		BorderBottom(true).
		Bold(true)
	s.Selected = s.Selected.
		Foreground(lipgloss.Color("229")).
		Background(lipgloss.Color("57")).
		Bold(false)
	t := table.New(
		table.WithColumns(cols),
		table.WithRows(rows),
		table.WithHeight(h),
		table.WithFocused(len(rows) > 0),
	)
	t.SetStyles(s)
	m.partTable = t
}

func (m model) updateDeviceDetail(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc", "backspace":
			m.view = viewDeviceList
			return m, nil
		case "w":
			dev := m.devices[m.selectedDev]
			if err := checkMountSafety(dev); err != nil {
				m.err = err.Error()
				return m, nil
			}
			m.err = ""
			m.wipeModeCursor = 0
			m.view = viewWipeMode
			return m, nil
		case "enter":
			dev := m.devices[m.selectedDev]
			idx := m.partTable.Cursor()
			if len(dev.Partitions) > 0 && idx >= 0 && idx < len(dev.Partitions) {
				m.browsePartIdx = idx
				m.view = viewFileBrowser
				return m, m.startBrowse(dev, idx)
			} else if dev.FSType != "" {
				m.browsePartIdx = -1
				m.view = viewFileBrowser
				return m, m.startBrowse(dev, -1)
			}
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.partTable, cmd = m.partTable.Update(msg)
	return m, cmd
}

func (m *model) startBrowse(dev USBDevice, partIdx int) tea.Cmd {
	var mountPoint string
	var partPath string

	if partIdx >= 0 {
		p := dev.Partitions[partIdx]
		mountPoint = p.MountPoint
		partPath = p.Path
	} else {
		// Whole device (superfloppy)
		mountPoint = dev.MountPoint
		partPath = dev.Path
	}

	if mountPoint != "" {
		// Already mounted — just read it
		m.browseDir = mountPoint
		m.browseMntPath = ""
		return readDirCmd(mountPoint)
	}

	// Need to mount it
	partName := filepath.Base(partPath)
	tmpDir := "/tmp/usbwipe-" + partName
	m.browseMntPath = tmpDir
	m.browseDir = tmpDir

	return func() tea.Msg {
		if err := os.MkdirAll(tmpDir, 0o755); err != nil {
			return mountResultMsg{err: err}
		}
		_, err := runCmd("mount", "-o", "ro", partPath, tmpDir)
		if err != nil {
			os.Remove(tmpDir)
			return mountResultMsg{err: fmt.Errorf("mount %s: %w", partPath, err)}
		}
		return mountResultMsg{path: tmpDir}
	}
}

func (m model) renderDeviceDetail() string {
	dev := m.devices[m.selectedDev]
	var b strings.Builder
	b.WriteString(titleStyle.Render("USB Wipe — Device Detail"))
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("  Device:  %s\n", dev.Path))
	b.WriteString(fmt.Sprintf("  Model:   %s %s\n", dev.Vendor, dev.Model))
	b.WriteString(fmt.Sprintf("  Size:    %s\n", dev.SizeHuman))

	if len(dev.Partitions) > 0 || dev.FSType != "" {
		b.WriteString("\n")
		b.WriteString(m.partTable.View())
	} else {
		b.WriteString("\n  No partitions or filesystem detected.\n")
	}

	if m.err != "" {
		b.WriteString("\n" + errStyle.Render("  Error: "+m.err))
	}
	b.WriteString("\n\n")
	help := "  esc back • w wipe"
	if len(dev.Partitions) > 0 || dev.FSType != "" {
		help += " • ↑/↓ navigate • enter browse"
	}
	b.WriteString(helpStyle.Render(help))
	return b.String()
}

// ── File Browser ─────────────────────────────────────────────────────────────

func (m model) updateFileBrowser(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case mountResultMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
			m.view = viewDeviceDetail
			return m, nil
		}
		return m, readDirCmd(msg.path)

	case dirListingMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, m.cleanupBrowse()
		}
		m.err = ""
		m.browseEntries = parseDirEntries(msg.entries)
		m.browseCursor = 0
		m.view = viewFileBrowser
		return m, nil

	case unmountDoneMsg:
		m.view = viewDeviceDetail
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Sequence(m.cleanupBrowse(), tea.Quit)
		case "esc":
			return m, m.cleanupBrowse()
		case "up", "k":
			if m.browseCursor > 0 {
				m.browseCursor--
			}
			return m, nil
		case "down", "j":
			if m.browseCursor < len(m.browseEntries)-1 {
				m.browseCursor++
			}
			return m, nil
		case "enter":
			if m.browseCursor < len(m.browseEntries) {
				entry := m.browseEntries[m.browseCursor]
				if entry.isDir {
					newDir := filepath.Join(m.browseDir, entry.name)
					m.browseDir = newDir
					return m, readDirCmd(newDir)
				}
			}
			return m, nil
		case "backspace":
			// Go to parent, or exit if at mount root
			mntRoot := m.browseMntPath
			if mntRoot == "" {
				// Using existing mount point
				dev := m.devices[m.selectedDev]
				if m.browsePartIdx >= 0 {
					mntRoot = dev.Partitions[m.browsePartIdx].MountPoint
				} else {
					mntRoot = dev.MountPoint
				}
			}
			if m.browseDir == mntRoot {
				return m, m.cleanupBrowse()
			}
			m.browseDir = filepath.Dir(m.browseDir)
			return m, readDirCmd(m.browseDir)
		}
	}
	return m, nil
}

func (m *model) cleanupBrowse() tea.Cmd {
	if m.browseMntPath == "" {
		m.view = viewDeviceDetail
		return nil
	}
	mnt := m.browseMntPath
	m.browseMntPath = ""
	return func() tea.Msg {
		runCmd("umount", mnt)
		os.Remove(mnt)
		return unmountDoneMsg{}
	}
}

func parseDirEntries(entries []fs.DirEntry) []fileEntry {
	result := make([]fileEntry, 0, len(entries))
	for _, e := range entries {
		fe := fileEntry{
			name:  e.Name(),
			isDir: e.IsDir(),
		}
		if info, err := e.Info(); err == nil {
			fe.size = info.Size()
			fe.mode = info.Mode()
			fe.modTime = info.ModTime().Format("2006-01-02 15:04")
		}
		result = append(result, fe)
	}
	// Dirs first, then alphabetical
	sort.Slice(result, func(i, j int) bool {
		if result[i].isDir != result[j].isDir {
			return result[i].isDir
		}
		return result[i].name < result[j].name
	})
	return result
}

func (m model) renderFileBrowser() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("USB Wipe — File Browser"))
	b.WriteString("\n")
	b.WriteString(dimStyle.Render("  " + m.browseDir))
	b.WriteString("\n\n")

	if len(m.browseEntries) == 0 {
		b.WriteString("  (empty directory)\n")
	} else {
		// Determine visible window
		visibleLines := m.height - 8
		if visibleLines < 5 {
			visibleLines = 5
		}
		start := 0
		if m.browseCursor >= visibleLines {
			start = m.browseCursor - visibleLines + 1
		}
		end := start + visibleLines
		if end > len(m.browseEntries) {
			end = len(m.browseEntries)
		}

		for i := start; i < end; i++ {
			e := m.browseEntries[i]
			prefix := "  "
			if i == m.browseCursor {
				prefix = "> "
			}

			name := e.name
			if e.isDir {
				name += "/"
			}

			sizeStr := humanSize(e.size)
			if e.isDir {
				sizeStr = "   <DIR>"
			}

			line := fmt.Sprintf("%s%-4s  %10s  %s  %s", prefix, e.mode.String()[:4], sizeStr, e.modTime, name)
			if i == m.browseCursor {
				line = selectedStyle.Render(line)
			}
			b.WriteString(line + "\n")
		}
	}

	if m.err != "" {
		b.WriteString("\n" + errStyle.Render("  Error: "+m.err))
	}
	b.WriteString("\n")
	b.WriteString(helpStyle.Render("  ↑/↓ navigate • enter open dir • backspace parent • esc exit"))
	return b.String()
}

// ── Wipe Mode ────────────────────────────────────────────────────────────────

func (m model) updateWipeMode(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			m.view = viewDeviceDetail
			return m, nil
		case "up", "k":
			if m.wipeModeCursor > 0 {
				m.wipeModeCursor--
			}
			return m, nil
		case "down", "j":
			if m.wipeModeCursor < len(wipeModeLabels)-1 {
				m.wipeModeCursor++
			}
			return m, nil
		case "enter":
			m.wipeMode = wipeMode(m.wipeModeCursor)
			m.fsCursor = 0
			m.view = viewWipeFS
			return m, nil
		}
	}
	return m, nil
}

func (m model) renderWipeMode() string {
	dev := m.devices[m.selectedDev]
	var b strings.Builder
	b.WriteString(titleStyle.Render("USB Wipe — Wipe Mode"))
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("  Device: %s (%s %s, %s)\n\n", dev.Path, dev.Vendor, dev.Model, dev.SizeHuman))
	for i, label := range wipeModeLabels {
		cursor := "  "
		if i == m.wipeModeCursor {
			cursor = "> "
		}
		line := fmt.Sprintf("%s%s", cursor, label)
		desc := fmt.Sprintf("    %s", wipeModeDescs[i])
		if i == m.wipeModeCursor {
			line = selectedStyle.Render(line)
			desc = selectedStyle.Render(desc)
		} else {
			desc = dimStyle.Render(desc)
		}
		b.WriteString(line + "\n")
		b.WriteString(desc + "\n")
	}
	b.WriteString("\n")
	b.WriteString(helpStyle.Render("  ↑/↓ navigate • enter select • esc cancel"))
	return b.String()
}

// ── Wipe Filesystem ──────────────────────────────────────────────────────────

func (m model) updateWipeFS(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			m.view = viewWipeMode
			return m, nil
		case "up", "k":
			if m.fsCursor > 0 {
				m.fsCursor--
			}
			return m, nil
		case "down", "j":
			if m.fsCursor < len(fsTypeLabels)-1 {
				m.fsCursor++
			}
			return m, nil
		case "enter":
			m.fsType = fsType(m.fsCursor)
			m.labelInput.SetValue("USB")
			m.labelInput.Focus()
			m.view = viewWipeLabel
			return m, nil
		}
	}
	return m, nil
}

func (m model) renderWipeFS() string {
	dev := m.devices[m.selectedDev]
	var b strings.Builder
	b.WriteString(titleStyle.Render("USB Wipe — Filesystem"))
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("  Device: %s (%s %s, %s)\n", dev.Path, dev.Vendor, dev.Model, dev.SizeHuman))
	b.WriteString(fmt.Sprintf("  Mode:   %s\n\n", wipeModeLabels[m.wipeMode]))
	for i, label := range fsTypeLabels {
		cursor := "  "
		if i == m.fsCursor {
			cursor = "> "
		}
		line := fmt.Sprintf("%s%s", cursor, label)
		desc := fmt.Sprintf("    %s", fsTypeDescs[i])
		if i == m.fsCursor {
			line = selectedStyle.Render(line)
			desc = selectedStyle.Render(desc)
		} else {
			desc = dimStyle.Render(desc)
		}
		b.WriteString(line + "\n")
		b.WriteString(desc + "\n")
	}
	b.WriteString("\n")
	b.WriteString(helpStyle.Render("  ↑/↓ navigate • enter select • esc back"))
	return b.String()
}

// ── Wipe Label ───────────────────────────────────────────────────────────────

func (m model) labelMaxLen() int {
	if m.fsType == fsExFAT {
		return 15
	}
	return 11
}

func (m model) updateWipeLabel(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			m.view = viewWipeFS
			return m, nil
		case "enter":
			val := m.labelInput.Value()
			maxLen := m.labelMaxLen()
			if val == "" || len(val) > maxLen {
				m.err = fmt.Sprintf("Label must be 1-%d characters", maxLen)
				return m, nil
			}
			m.err = ""
			m.confirmInput.SetValue("")
			m.confirmInput.Focus()
			m.view = viewWipeConfirm
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.labelInput, cmd = m.labelInput.Update(msg)
	return m, cmd
}

func (m model) renderWipeLabel() string {
	var b strings.Builder
	dev := m.devices[m.selectedDev]
	b.WriteString(titleStyle.Render("USB Wipe — Volume Label"))
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("  Device: %s (%s %s, %s)\n", dev.Path, dev.Vendor, dev.Model, dev.SizeHuman))
	b.WriteString(fmt.Sprintf("  Mode:   %s\n", wipeModeLabels[m.wipeMode]))
	b.WriteString(fmt.Sprintf("  FS:     %s\n\n", fsTypeLabels[m.fsType]))
	b.WriteString(fmt.Sprintf("  Volume label (max %d chars):\n\n", m.labelMaxLen()))
	b.WriteString("  " + m.labelInput.View())
	if m.err != "" {
		b.WriteString("\n\n" + errStyle.Render("  "+m.err))
	}
	b.WriteString("\n\n")
	b.WriteString(helpStyle.Render("  enter continue • esc back"))
	return b.String()
}

// ── Wipe Confirm ─────────────────────────────────────────────────────────────

func (m model) updateWipeConfirm(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			m.view = viewWipeLabel
			return m, nil
		case "enter":
			if strings.TrimSpace(strings.ToLower(m.confirmInput.Value())) != "yes" {
				m.err = "Type 'yes' to confirm"
				return m, nil
			}
			m.err = ""
			m.view = viewWiping
			dev := m.devices[m.selectedDev]
			label := strings.ToUpper(m.labelInput.Value())
			return m, tea.Batch(m.spinner.Tick, wipeCmd(dev, label, m.wipeMode, m.fsType))
		}
	}
	var cmd tea.Cmd
	m.confirmInput, cmd = m.confirmInput.Update(msg)
	return m, cmd
}

func (m model) renderWipeConfirm() string {
	var b strings.Builder
	dev := m.devices[m.selectedDev]
	b.WriteString(titleStyle.Render("USB Wipe — Confirm"))
	b.WriteString("\n\n")
	b.WriteString(errStyle.Render(fmt.Sprintf("  ⚠ WIPE ALL DATA on %s (%s %s, %s)?", dev.Path, dev.Vendor, dev.Model, dev.SizeHuman)))
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("  Mode:  %s\n", wipeModeLabels[m.wipeMode]))
	b.WriteString(fmt.Sprintf("  FS:    %s\n", fsTypeLabels[m.fsType]))
	b.WriteString(fmt.Sprintf("  Label: %s\n\n", strings.ToUpper(m.labelInput.Value())))
	b.WriteString("  Type 'yes' to confirm:\n\n")
	b.WriteString("  " + m.confirmInput.View())
	if m.err != "" {
		b.WriteString("\n\n" + errStyle.Render("  "+m.err))
	}
	b.WriteString("\n\n")
	b.WriteString(helpStyle.Render("  enter confirm • esc back"))
	return b.String()
}

// ── Wiping ───────────────────────────────────────────────────────────────────

func (m model) updateWiping(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case wipeResultMsg:
		m.wipeErr = msg.err
		m.view = viewWipeDone
		return m, nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m model) renderWiping() string {
	var b strings.Builder
	dev := m.devices[m.selectedDev]
	b.WriteString(titleStyle.Render("USB Wipe — Wiping"))
	b.WriteString("\n\n")
	step := "Wiping"
	switch m.wipeMode {
	case wipeQuickVerify:
		step = "Wiping (quick verify)"
	case wipeFullVerify:
		step = "Wiping (full verify — this may take a while)"
	}
	b.WriteString(fmt.Sprintf("  %s %s %s...", m.spinner.View(), step, dev.Path))
	b.WriteString("\n\n")
	b.WriteString(helpStyle.Render("  Please wait..."))
	return b.String()
}

// ── Wipe Done ────────────────────────────────────────────────────────────────

func (m model) updateWipeDone(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case ejectResultMsg:
		if msg.err != nil {
			m.ejectErr = msg.err
			return m, nil
		}
		return m, tea.Quit
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "enter":
			return m, tea.Quit
		case "e":
			if m.wipeErr == nil {
				dev := m.devices[m.selectedDev]
				return m, ejectCmd(dev)
			}
			return m, nil
		case "esc":
			// Refresh devices and go back to list
			m.wipeErr = nil
			m.ejectErr = nil
			m.err = ""
			return m, detectCmd
		}
	case devicesRefreshedMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
		} else {
			m.devices = msg.devices
			m.table.SetRows(devicesToRows(m.devices))
			m.table.SetHeight(len(m.devices) + 1)
		}
		m.view = viewDeviceList
		return m, nil
	}
	return m, nil
}

func (m model) renderWipeDone() string {
	var b strings.Builder
	dev := m.devices[m.selectedDev]
	b.WriteString(titleStyle.Render("USB Wipe — Complete"))
	b.WriteString("\n\n")
	if m.wipeErr != nil {
		b.WriteString(errStyle.Render(fmt.Sprintf("  Wipe FAILED: %v", m.wipeErr)))
		b.WriteString("\n\n")
		b.WriteString(helpStyle.Render("  esc back to list • q quit"))
	} else {
		b.WriteString(successStyle.Render(fmt.Sprintf("  ✓ Successfully wiped %s", dev.Path)))
		b.WriteString("\n")
		b.WriteString(fmt.Sprintf("  Label: %s\n", strings.ToUpper(m.labelInput.Value())))
		if m.ejectErr != nil {
			b.WriteString("\n" + errStyle.Render(fmt.Sprintf("  Eject failed: %v", m.ejectErr)))
		}
		b.WriteString("\n")
		b.WriteString(helpStyle.Render("  e eject & quit • enter/q quit • esc back to list"))
	}
	return b.String()
}

// ── View ─────────────────────────────────────────────────────────────────────

func (m model) View() string {
	switch m.view {
	case viewDeviceList:
		return m.renderDeviceList()
	case viewDeviceDetail:
		return m.renderDeviceDetail()
	case viewFileBrowser:
		return m.renderFileBrowser()
	case viewWipeMode:
		return m.renderWipeMode()
	case viewWipeFS:
		return m.renderWipeFS()
	case viewWipeLabel:
		return m.renderWipeLabel()
	case viewWipeConfirm:
		return m.renderWipeConfirm()
	case viewWiping:
		return m.renderWiping()
	case viewWipeDone:
		return m.renderWipeDone()
	}
	return ""
}

// ── Async commands ───────────────────────────────────────────────────────────

func detectCmd() tea.Msg {
	devices, err := detectUSBDevices()
	return devicesRefreshedMsg{devices: devices, err: err}
}

func readDirCmd(path string) tea.Cmd {
	return func() tea.Msg {
		entries, err := os.ReadDir(path)
		return dirListingMsg{entries: entries, err: err}
	}
}

func wipeCmd(dev USBDevice, label string, mode wipeMode, fs fsType) tea.Cmd {
	return func() tea.Msg {
		err := doWipe(dev, label, mode, fs)
		return wipeResultMsg{err: err}
	}
}

func ejectCmd(dev USBDevice) tea.Cmd {
	return func() tea.Msg {
		_, err := runCmd("eject", dev.Path)
		return ejectResultMsg{err: err}
	}
}

// ── Backend functions ────────────────────────────────────────────────────────

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

		removable := readSysfs(base + "/removable")
		logv("  removable = %q", removable)
		if removable != "1" {
			logv("  skipped: not removable")
			continue
		}

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

func humanSize(b int64) string {
	const (
		KB = 1000
		MB = 1000 * KB
		GB = 1000 * MB
		TB = 1000 * GB
	)
	switch {
	case b >= TB:
		return fmt.Sprintf("%.1f TB", float64(b)/float64(TB))
	case b >= GB:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(GB))
	case b >= MB:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(MB))
	default:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(KB))
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

func doWipe(dev USBDevice, label string, mode wipeMode, fstype fsType) error {
	// Unmount everything
	if dev.MountPoint != "" {
		if out, err := runCmd("umount", dev.Path); err != nil {
			return fmt.Errorf("umount %s: %w\n%s", dev.Path, err, out)
		}
	}
	for _, p := range dev.Partitions {
		if p.MountPoint != "" {
			if out, err := runCmd("umount", p.Path); err != nil {
				return fmt.Errorf("umount %s: %w\n%s", p.Path, err, out)
			}
		}
	}

	// Full verify: run badblocks destructive write test on whole device
	if mode == wipeFullVerify {
		if out, err := runCmd("badblocks", "-w", "-s", dev.Path); err != nil {
			return fmt.Errorf("badblocks: %w\n%s", err, out)
		}
	}

	// Wipe filesystem signatures
	if out, err := runCmd("wipefs", "-a", dev.Path); err != nil {
		return fmt.Errorf("wipefs: %w\n%s", err, out)
	}

	// Partition table
	var sfdiskInput string
	switch fstype {
	case fsExFAT:
		sfdiskInput = "label: dos\ntype=7\n" // type 7 = NTFS/exFAT
	default:
		sfdiskInput = "label: dos\ntype=c\n" // type c = FAT32 LBA
	}
	if out, err := runCmdStdin("sfdisk", sfdiskInput, "--lock", dev.Path); err != nil {
		return fmt.Errorf("sfdisk: %w\n%s", err, out)
	}

	// Format
	part1 := dev.Path + "1"
	switch fstype {
	case fsExFAT:
		args := []string{"-n", label, part1}
		if mode == wipeQuickVerify {
			// mkfs.exfat doesn't support -c; use badblocks read-only check instead
			if out, err := runCmd("badblocks", "-s", part1); err != nil {
				return fmt.Errorf("badblocks (quick check): %w\n%s", err, out)
			}
		}
		if out, err := runCmd("mkfs.exfat", args...); err != nil {
			return fmt.Errorf("mkfs.exfat: %w\n%s", err, out)
		}
	default:
		args := []string{"-F", "32", "-n", label}
		if mode == wipeQuickVerify {
			args = append(args, "-c")
		}
		args = append(args, part1)
		if out, err := runCmd("mkfs.vfat", args...); err != nil {
			return fmt.Errorf("mkfs.vfat: %w\n%s", err, out)
		}
	}

	return nil
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

// ── Main ─────────────────────────────────────────────────────────────────────

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

	m := newModel(devices)
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
