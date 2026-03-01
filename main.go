package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io/fs"
	"log/syslog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
	"unsafe"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
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
	wipeSecureZero
	wipeSecureRandom
)

var wipeModeLabels = []string{
	"Wipe",
	"Wipe + Quick Verify",
	"Wipe + Full Verify",
	"Secure Wipe (Zero)",
	"Secure Wipe (Random)",
}

var wipeModeDescs = []string{
	"Reformat only (fastest)",
	"Reformat with bad sector check",
	"Full surface scan with badblocks, then reformat (slow)",
	"Overwrite entire device with zeros, then reformat",
	"Overwrite entire device with random data, then reformat",
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

type wipeOutputMsg struct{ line string }

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
	wipeOutput   []string
	wipeState    *wipeState
	wipeViewport viewport.Model
	wipeErr      error
	confirmAbort bool

	// Eject
	ejectErr error
}

// ── Styles ───────────────────────────────────────────────────────────────────

var borderColor = lipgloss.Color("63")

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

	cmdStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8"))

	borderFg = lipgloss.NewStyle().Foreground(borderColor)
)

// renderPane draws a rounded-border box with a title inset into the top border,
// and a help bar below.
func (m model) renderPane(title, body, help string) string {
	innerW := m.width - 4 // 2 for border chars + 2 for padding
	if innerW < 36 {
		innerW = 36
	}

	// Pad the body content
	padded := lipgloss.NewStyle().Width(innerW).Padding(0, 1).Render(body)

	// Build box
	border := lipgloss.RoundedBorder()
	hBar := []rune(strings.Repeat(border.Top, innerW+2)) // +2 for inner padding
	titleStr := titleStyle.Render(" " + title + " ")
	titleW := lipgloss.Width(titleStr)

	// Top border with title spliced in (use rune slicing — border chars are multi-byte UTF-8)
	var top string
	if titleW+2 < len(hBar) {
		top = borderFg.Render(string(border.TopLeft)) +
			borderFg.Render(string(hBar[:1])) +
			titleStr +
			borderFg.Render(string(hBar[1+titleW:])) +
			borderFg.Render(string(border.TopRight))
	} else {
		top = borderFg.Render(string(border.TopLeft) + string(hBar) + string(border.TopRight))
	}

	// Bottom border
	bBar := string([]rune(strings.Repeat(border.Bottom, innerW+2)))
	bottom := borderFg.Render(string(border.BottomLeft) + bBar + string(border.BottomRight))

	// Side borders on each line
	left := borderFg.Render(border.Left)
	right := borderFg.Render(border.Right)
	var mid strings.Builder
	for _, line := range strings.Split(padded, "\n") {
		// Pad line to full width
		lineW := lipgloss.Width(line)
		pad := ""
		if lineW < innerW+2 {
			pad = strings.Repeat(" ", innerW+2-lineW)
		}
		mid.WriteString(left + line + pad + right + "\n")
	}

	return top + "\n" + mid.String() + bottom + "\n" + helpStyle.Render(" "+help)
}

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
	if msg, ok := msg.(tea.WindowSizeMsg); ok {
		m.width = msg.Width
		m.height = msg.Height
		if m.view == viewWiping {
			m.wipeViewport.Width = msg.Width
			m.wipeViewport.Height = msg.Height - 6
		}
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
	if len(m.devices) == 0 {
		b.WriteString("No USB devices found.\n")
	} else {
		b.WriteString(m.table.View())
	}
	if m.err != "" {
		b.WriteString("\n" + errStyle.Render("Error: "+m.err))
	}
	return m.renderPane("Device List", b.String(), "↑/↓ navigate • enter select • r refresh • q/esc quit")
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
	var fstype string

	if partIdx >= 0 {
		p := dev.Partitions[partIdx]
		mountPoint = p.MountPoint
		partPath = p.Path
		fstype = p.FSType
	} else {
		// Whole device (superfloppy)
		mountPoint = dev.MountPoint
		partPath = dev.Path
		fstype = dev.FSType
	}

	if mountPoint != "" {
		// Already mounted — just read it
		m.browseDir = mountPoint
		m.browseMntPath = ""
		return readDirCmd(mountPoint)
	}

	// Need to mount it — use unpredictable temp dir to prevent symlink attacks
	m.browseMntPath = "" // set after successful MkdirTemp
	m.browseDir = ""

	return func() tea.Msg {
		tmpDir, err := os.MkdirTemp("", "usbwipe-")
		if err != nil {
			return mountResultMsg{err: err}
		}
		args := []string{"-o", "ro"}
		if fstype != "" {
			args = append(args, "-t", fstype)
		}
		args = append(args, partPath, tmpDir)
		out, err := runCmd("mount", args...)
		if err != nil {
			os.Remove(tmpDir)
			return mountResultMsg{err: fmt.Errorf("mount %s: %w\n%s", partPath, err, strings.TrimSpace(out))}
		}
		return mountResultMsg{path: tmpDir}
	}
}

func (m model) renderDeviceDetail() string {
	dev := m.devices[m.selectedDev]
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Device:  %s\n", dev.Path))
	b.WriteString(fmt.Sprintf("Model:   %s %s\n", dev.Vendor, dev.Model))
	b.WriteString(fmt.Sprintf("Size:    %s\n", dev.SizeHuman))

	if len(dev.Partitions) > 0 || dev.FSType != "" {
		b.WriteString("\n")
		b.WriteString(m.partTable.View())
	} else {
		b.WriteString("\nNo partitions or filesystem detected.\n")
	}

	if m.err != "" {
		b.WriteString("\n" + errStyle.Render("Error: "+m.err))
	}
	help := "esc back • w wipe"
	if len(dev.Partitions) > 0 || dev.FSType != "" {
		help += " • ↑/↓ navigate • enter browse"
	}
	return m.renderPane("Device Detail", b.String(), help)
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
		// If user navigated away while mount was in progress, clean up
		if m.view != viewFileBrowser {
			go func() {
				runCmd("umount", msg.path)
				os.Remove(msg.path)
			}()
			return m, nil
		}
		m.browseMntPath = msg.path
		m.browseDir = msg.path
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
		if msg.err != nil {
			m.err = fmt.Sprintf("unmount warning: %v", msg.err)
		}
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
		_, err := runCmd("umount", mnt)
		os.Remove(mnt)
		return unmountDoneMsg{err: err}
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
	b.WriteString(dimStyle.Render(m.browseDir))
	b.WriteString("\n\n")

	if len(m.browseEntries) == 0 {
		b.WriteString("(empty directory)\n")
	} else {
		visibleLines := m.height - 10
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
		b.WriteString("\n" + errStyle.Render("Error: "+m.err))
	}
	return m.renderPane("File Browser", b.String(), "↑/↓ navigate • enter open dir • backspace parent • esc exit")
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
	b.WriteString(fmt.Sprintf("Device: %s (%s %s, %s)\n\n", dev.Path, dev.Vendor, dev.Model, dev.SizeHuman))
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
	return m.renderPane("Wipe Mode", b.String(), "↑/↓ navigate • enter select • esc cancel")
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
	b.WriteString(fmt.Sprintf("Device: %s (%s %s, %s)\n", dev.Path, dev.Vendor, dev.Model, dev.SizeHuman))
	b.WriteString(fmt.Sprintf("Mode:   %s\n\n", wipeModeLabels[m.wipeMode]))
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
	return m.renderPane("Filesystem", b.String(), "↑/↓ navigate • enter select • esc back")
}

// ── Wipe Label ───────────────────────────────────────────────────────────────

func (m model) labelMaxLen() int {
	if m.fsType == fsExFAT {
		return 15 // exFAT spec: max 15 characters (spec measures UTF-16 code units; BMP-only in practice)
	}
	return 11 // FAT32 spec: 11-byte volume label field in directory entry
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
			if val == "" || utf8.RuneCountInString(val) > maxLen {
				m.err = fmt.Sprintf("Label must be 1-%d characters", maxLen)
				return m, nil
			}
			// Validate label characters (reject control chars and invalid FS characters)
			upper := strings.ToUpper(val)
			if m.fsType == fsFAT32 {
				// Intentionally more restrictive than the FAT32 spec (which allows
				// chars like !#$&). Conservative allowlist avoids shell-quoting
				// surprises and cross-platform compatibility issues.
				for _, r := range upper {
					if !((r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == ' ' || r == '_' || r == '-') {
						m.err = "FAT32 label: A-Z, 0-9, space, underscore, hyphen only"
						return m, nil
					}
				}
			} else {
				for _, r := range val {
					if r < 0x20 || r == 0x7F {
						m.err = "Label must not contain control characters"
						return m, nil
					}
				}
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
	dev := m.devices[m.selectedDev]
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Device: %s (%s %s, %s)\n", dev.Path, dev.Vendor, dev.Model, dev.SizeHuman))
	b.WriteString(fmt.Sprintf("Mode:   %s\n", wipeModeLabels[m.wipeMode]))
	b.WriteString(fmt.Sprintf("FS:     %s\n\n", fsTypeLabels[m.fsType]))
	b.WriteString(fmt.Sprintf("Volume label (max %d chars):\n\n", m.labelMaxLen()))
	b.WriteString(m.labelInput.View())
	if m.err != "" {
		b.WriteString("\n\n" + errStyle.Render(m.err))
	}
	return m.renderPane("Volume Label", b.String(), "enter continue • esc back")
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
			m.wipeOutput = nil
			m.wipeErr = nil
			dev := m.devices[m.selectedDev]
			label := strings.ToUpper(m.labelInput.Value())
			m.wipeState = startWipe(dev, label, m.wipeMode, m.fsType)
			m.wipeViewport = viewport.New(m.width, m.height-6)
			return m, tea.Batch(m.spinner.Tick, waitForWipeOutput(m.wipeState))
		}
	}
	var cmd tea.Cmd
	m.confirmInput, cmd = m.confirmInput.Update(msg)
	return m, cmd
}

func (m model) renderWipeConfirm() string {
	dev := m.devices[m.selectedDev]
	var b strings.Builder
	b.WriteString(errStyle.Render(fmt.Sprintf("⚠ WIPE ALL DATA on %s (%s %s, %s)?", dev.Path, dev.Vendor, dev.Model, dev.SizeHuman)))
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("Mode:  %s\n", wipeModeLabels[m.wipeMode]))
	b.WriteString(fmt.Sprintf("FS:    %s\n", fsTypeLabels[m.fsType]))
	b.WriteString(fmt.Sprintf("Label: %s\n\n", strings.ToUpper(m.labelInput.Value())))
	b.WriteString("Type 'yes' to confirm:\n\n")
	b.WriteString(m.confirmInput.View())
	if m.err != "" {
		b.WriteString("\n\n" + errStyle.Render(m.err))
	}
	return m.renderPane("Confirm", b.String(), "enter confirm • esc back")
}

// ── Wiping ───────────────────────────────────────────────────────────────────

func (m model) updateWiping(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Abort confirmation overlay
	if m.confirmAbort {
		if msg, ok := msg.(tea.KeyMsg); ok {
			switch msg.String() {
			case "y", "Y":
				if m.wipeState != nil {
					m.wipeState.Kill()
				}
				return m, tea.Quit
			case "n", "N", "esc":
				m.confirmAbort = false
				return m, nil
			}
		}
		// While overlay is shown, still process spinner and output
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			m.confirmAbort = true
			return m, nil
		}
	case wipeOutputMsg:
		line := msg.line
		// Lines prefixed with \r overwrite the last output line (progress counters)
		if strings.HasPrefix(line, "\r") {
			line = line[1:]
			if len(m.wipeOutput) > 0 {
				m.wipeOutput[len(m.wipeOutput)-1] = line
			} else {
				m.wipeOutput = append(m.wipeOutput, line)
			}
		} else {
			m.wipeOutput = append(m.wipeOutput, line)
		}
		m.wipeViewport.SetContent(renderWipeOutput(m.wipeOutput))
		m.wipeViewport.GotoBottom()
		return m, waitForWipeOutput(m.wipeState)
	case wipeResultMsg:
		m.wipeErr = msg.err
		m.confirmAbort = false
		m.view = viewWipeDone
		return m, nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}
	return m, nil
}

// renderWipeOutput styles wipe log lines. Lines prefixed with \x00 are
// command echoes (injected by runCmdStdinStream) and rendered with cmdStyle;
// the prefix is stripped before display.
func renderWipeOutput(lines []string) string {
	styled := make([]string, len(lines))
	for i, line := range lines {
		if strings.HasPrefix(line, "\x00") {
			styled[i] = cmdStyle.Render(line[1:])
		} else {
			styled[i] = line
		}
	}
	return strings.Join(styled, "\n")
}

func (m model) renderWiping() string {
	dev := m.devices[m.selectedDev]
	step := "Wiping"
	switch m.wipeMode {
	case wipeQuickVerify:
		step = "Wiping (quick verify)"
	case wipeFullVerify:
		step = "Wiping (full verify)"
	case wipeSecureZero:
		step = "Secure wiping (zero)"
	case wipeSecureRandom:
		step = "Secure wiping (random)"
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%s %s %s...\n\n", m.spinner.View(), step, dev.Path))
	b.WriteString(m.wipeViewport.View())

	if m.confirmAbort {
		return m.renderPane("Wiping", b.String(), errStyle.Render("Abort wipe? May leave drive unusable.")+" y abort • n continue")
	}
	return m.renderPane("Wiping", b.String(), "ctrl+c abort")
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
	dev := m.devices[m.selectedDev]
	var b strings.Builder
	var help string
	if m.wipeErr != nil {
		b.WriteString(errStyle.Render(fmt.Sprintf("Wipe FAILED: %v", m.wipeErr)))
		help = "esc back to list • q quit"
	} else {
		b.WriteString(successStyle.Render(fmt.Sprintf("✓ Successfully wiped %s", dev.Path)))
		b.WriteString("\n")
		b.WriteString(fmt.Sprintf("Label: %s\n", strings.ToUpper(m.labelInput.Value())))
		if m.ejectErr != nil {
			b.WriteString("\n" + errStyle.Render(fmt.Sprintf("Eject failed: %v", m.ejectErr)))
		}
		help = "e eject & quit • enter/q quit • esc back to list"
	}
	return m.renderPane("Complete", b.String(), help)
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

type wipeState struct {
	ch  chan wipeOutputMsg
	err error
	mu  sync.Mutex
	cmd *exec.Cmd // currently running subprocess
}

func (ws *wipeState) Kill() {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.cmd != nil && ws.cmd.Process != nil {
		// Send SIGTERM to the process group
		syscall.Kill(-ws.cmd.Process.Pid, syscall.SIGTERM)
	}
}

func startWipe(dev USBDevice, label string, mode wipeMode, fs fsType) *wipeState {
	ws := &wipeState{ch: make(chan wipeOutputMsg, 64)}
	go func() {
		ws.err = doWipe(dev, label, mode, fs, ws)
		close(ws.ch)
	}()
	return ws
}

func waitForWipeOutput(ws *wipeState) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ws.ch
		if !ok {
			return wipeResultMsg{err: ws.err}
		}
		return msg
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

		// /sys/block/*/size is always in 512-byte units per Linux kernel ABI,
		// regardless of the device's physical sector size.
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
			if mps, ok := mounts[pPath]; ok && len(mps) > 0 {
				part.MountPoint = mps[0]
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
		if mps, ok := mounts[dev.Path]; ok && len(mps) > 0 {
			dev.MountPoint = mps[0]
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

func parseProcMounts() map[string][]string {
	mounts := make(map[string][]string)
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return mounts
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 {
			mounts[fields[0]] = append(mounts[fields[0]], fields[1])
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
	critical := map[string]bool{
		"/": true, "/boot": true, "/home": true,
		"/usr": true, "/var": true, "/etc": true,
		"/srv": true, "/opt": true, "/tmp": true,
	}
	// Re-read current mounts to catch changes since detection
	mounts := parseProcMounts()
	paths := []string{dev.Path}
	for _, p := range dev.Partitions {
		paths = append(paths, p.Path)
	}
	for _, devPath := range paths {
		for _, mp := range mounts[devPath] {
			for critPath := range critical {
				if mp == critPath || strings.HasPrefix(mp, critPath+"/") {
					return fmt.Errorf("refusing to wipe: %s is mounted at %s", devPath, mp)
				}
			}
		}
	}
	return nil
}

func doWipe(dev USBDevice, label string, mode wipeMode, fstype fsType, ws *wipeState) (retErr error) {
	ch := ws.ch
	emit := func(format string, args ...any) {
		ch <- wipeOutputMsg{line: fmt.Sprintf(format, args...)}
	}

	// Re-verify device identity to guard against TOCTOU (device unplugged/replugged
	// with a different device receiving the same /dev/sdX path)
	base := "/sys/block/" + dev.Name
	curModel := strings.TrimSpace(readSysfs(base + "/device/model"))
	curVendor := strings.TrimSpace(readSysfs(base + "/device/vendor"))
	var curSectors int64
	fmt.Sscanf(readSysfs(base+"/size"), "%d", &curSectors)
	if curModel != dev.Model || curVendor != dev.Vendor || curSectors != dev.SizeSectors {
		return fmt.Errorf("device %s identity changed since selection (expected %s %s, got %s %s) — was it unplugged?",
			dev.Path, dev.Vendor, dev.Model, curVendor, curModel)
	}

	// Re-check mount safety (mounts may have changed since the user pressed 'w')
	if err := checkMountSafety(dev); err != nil {
		return err
	}

	// Audit log — persistent record of destructive operations (best-effort;
	// if syslog is unavailable the wipe proceeds with a warning).
	if sl, err := syslog.New(syslog.LOG_NOTICE|syslog.LOG_USER, "usbwipe"); err == nil {
		sl.Notice(fmt.Sprintf("WIPE START device=%s vendor=%q model=%q size=%s mode=%s fs=%s label=%q",
			dev.Path, dev.Vendor, dev.Model, dev.SizeHuman, wipeModeLabels[mode], fsTypeLabels[fstype], label))
		defer func() {
			if retErr != nil {
				sl.Err(fmt.Sprintf("WIPE FAILED device=%s err=%v", dev.Path, retErr))
			} else {
				sl.Notice(fmt.Sprintf("WIPE OK device=%s", dev.Path))
			}
			sl.Close()
		}()
	} else {
		emit("warning: audit log unavailable (syslog): %v", err)
	}

	// Unmount everything — re-read /proc/mounts for current state
	// (mounts may have changed since detection, e.g. auto-mount)
	mounts := parseProcMounts()
	devPaths := []string{dev.Path}
	for _, p := range dev.Partitions {
		devPaths = append(devPaths, p.Path)
	}
	for _, dp := range devPaths {
		for _, mp := range mounts[dp] {
			emit("Unmounting %s (%s)...", dp, mp)
			if err := runCmdStream("umount", ws, mp); err != nil {
				// Re-check — mount may have been removed by a prior umount (bind mounts)
				if fresh := parseProcMounts(); len(fresh[dp]) > 0 {
					return fmt.Errorf("umount %s: %w", mp, err)
				}
			}
		}
	}

	// Full verify: run badblocks destructive write test on whole device
	if mode == wipeFullVerify {
		emit("Running full surface scan (badblocks -w)...")
		if err := runCmdStream("badblocks", ws, "-w", "-s", "-v", dev.Path); err != nil {
			return fmt.Errorf("badblocks: %w", err)
		}
	}

	// Secure wipe: overwrite entire device with dd
	if mode == wipeSecureZero || mode == wipeSecureRandom {
		src := "/dev/zero"
		if mode == wipeSecureRandom {
			src = "/dev/urandom"
		}
		emit(fmt.Sprintf("Overwriting device with %s...", src))
		if err := runCmdStream("dd", ws, "if="+src, "of="+dev.Path, "bs=1M", "status=progress"); err != nil {
			// dd exits non-zero when it hits the end of the block device ("No space left
			// on device"). This is expected — it means the entire device was overwritten.
			if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
				// expected: dd wrote until the device was full
			} else {
				return fmt.Errorf("dd: %w", err)
			}
		}
	}

	// Wipe filesystem signatures
	emit("Wiping filesystem signatures...")
	if err := runCmdStream("wipefs", ws, "-a", dev.Path); err != nil {
		return fmt.Errorf("wipefs: %w", err)
	}

	// Partition table
	emit("Creating partition table...")
	var sfdiskInput string
	switch fstype {
	case fsExFAT:
		sfdiskInput = "label: dos\ntype=7\n" // type 7 = NTFS/exFAT
	default:
		sfdiskInput = "label: dos\ntype=c\n" // type c = FAT32 LBA
	}
	if err := runCmdStdinStream("sfdisk", sfdiskInput, ws, "--lock", dev.Path); err != nil {
		return fmt.Errorf("sfdisk: %w", err)
	}

	// Wait for kernel to pick up new partition table
	part1 := dev.Path + "1"
	emit("Waiting for %s to appear...", part1)
	runCmdStream("partprobe", ws, dev.Path)
	runCmdStream("udevadm", ws, "settle", "--timeout=5")
	// udevadm settle can return before the device node is actually created
	// (race between udev rule execution and devtmpfs node creation).
	// Poll as a fallback for slow USB controllers / hubs.
	for i := 0; i < 20; i++ {
		if _, err := os.Stat(part1); err == nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if _, err := os.Stat(part1); err != nil {
		return fmt.Errorf("partition %s did not appear after sfdisk", part1)
	}
	// Quick verify for exFAT: run badblocks read-only scan
	// (mkfs.exfat has no built-in -c flag unlike mkfs.vfat)
	if mode == wipeQuickVerify && fstype == fsExFAT {
		emit("Running quick bad block check on %s...", part1)
		if err := runCmdStream("badblocks", ws, "-s", "-v", part1); err != nil {
			return fmt.Errorf("badblocks: %w", err)
		}
	}

	switch fstype {
	case fsExFAT:
		emit("Formatting %s as exFAT (label: %s)...", part1, label)
		if err := runCmdStream("mkfs.exfat", ws, "-n", label, part1); err != nil {
			return fmt.Errorf("mkfs.exfat: %w", err)
		}
	default:
		if mode == wipeQuickVerify {
			emit("Formatting %s as FAT32 with bad block check (label: %s)...", part1, label)
			if err := runCmdStream("mkfs.vfat", ws, "-F", "32", "-n", label, "-c", part1); err != nil {
				return fmt.Errorf("mkfs.vfat: %w", err)
			}
		} else {
			emit("Formatting %s as FAT32 (label: %s)...", part1, label)
			if err := runCmdStream("mkfs.vfat", ws, "-F", "32", "-n", label, part1); err != nil {
				return fmt.Errorf("mkfs.vfat: %w", err)
			}
		}
	}

	emit("Done.")
	return nil
}

// openPTY allocates a pseudo-terminal pair. The slave side is passed to child
// processes so they see a real terminal (enabling progress output in tools like
// badblocks that use \b-based progress bars). The master side is used to read
// the child's output.
func openPTY() (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return
	}

	// Get slave PTY number
	var n uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(),
		syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); errno != 0 {
		master.Close()
		err = fmt.Errorf("TIOCGPTN: %v", errno)
		return
	}

	// Unlock slave
	var unlock int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(),
		syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		master.Close()
		err = fmt.Errorf("TIOCSPTLCK: %v", errno)
		return
	}

	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return
	}

	// Set raw mode: disable echo, canonical mode, signal generation,
	// and output processing (ONLCR converts \n→\r\n which we don't want).
	var attr syscall.Termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, slave.Fd(),
		syscall.TCGETS, uintptr(unsafe.Pointer(&attr))); errno != 0 {
		master.Close()
		slave.Close()
		err = fmt.Errorf("TCGETS: %v", errno)
		return
	}
	attr.Iflag &^= syscall.ICRNL | syscall.INLCR | syscall.IGNCR | syscall.IXON
	attr.Oflag &^= syscall.OPOST
	attr.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, slave.Fd(),
		syscall.TCSETS, uintptr(unsafe.Pointer(&attr))); errno != 0 {
		master.Close()
		slave.Close()
		err = fmt.Errorf("TCSETS: %v", errno)
		return
	}

	return
}

func runCmdStream(name string, ws *wipeState, args ...string) error {
	return runCmdStdinStream(name, "", ws, args...)
}

func runCmdStdinStream(name string, stdin string, ws *wipeState, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}

	// Log the command to the wipe output pane
	ws.ch <- wipeOutputMsg{line: "\x00$ " + name + " " + strings.Join(args, " ")}

	// Use a PTY so child processes see a terminal and flush progress output
	// immediately (badblocks checks isatty / uses line-buffered stderr on ttys).
	master, slave, err := openPTY()
	if err != nil {
		return fmt.Errorf("openpty: %w", err)
	}
	cmd.Stdout = slave
	cmd.Stderr = slave

	if err := cmd.Start(); err != nil {
		master.Close()
		slave.Close()
		return err
	}
	slave.Close() // close slave in parent; child has its own fd

	// Register the running command so it can be killed
	ws.mu.Lock()
	ws.cmd = cmd
	ws.mu.Unlock()

	ch := ws.ch

	// Process output byte-by-byte, handling \n, \r, and \b (backspace).
	// badblocks uses \b to overwrite progress in-place rather than \r.
	var readerWg sync.WaitGroup
	readerWg.Add(1)
	go func() {
		defer readerWg.Done()
		defer master.Close()
		buf := make([]byte, 4096)
		var curLine []byte     // current line buffer (content persists across \b)
		var cursor int         // write position within curLine
		var lastEmitted string // last progress string sent to channel

		emit := func(overwrite bool) {
			s := strings.TrimRight(string(curLine), " ")
			if s == "" {
				return
			}
			if overwrite {
				ch <- wipeOutputMsg{line: "\r" + s}
			} else {
				ch <- wipeOutputMsg{line: s}
			}
			lastEmitted = s
		}

		for {
			n, err := master.Read(buf)
			if n > 0 {
				for _, b := range buf[:n] {
					switch b {
					case '\n':
						emit(lastEmitted != "")
						curLine = curLine[:0]
						cursor = 0
						lastEmitted = ""
					case '\r':
						cursor = 0
					case '\b':
						if cursor > 0 {
							cursor--
						}
					default:
						if cursor < len(curLine) {
							curLine[cursor] = b // overwrite at cursor
						} else {
							curLine = append(curLine, b)
						}
						cursor++
					}
				}
				// Flush in-progress line so progress is visible immediately
				s := strings.TrimRight(string(curLine), " ")
				if s != "" && s != lastEmitted {
					emit(lastEmitted != "")
				}
			}
			if err != nil {
				break
			}
		}
		s := strings.TrimRight(string(curLine), " ")
		if s != "" && s != lastEmitted {
			ch <- wipeOutputMsg{line: s}
		}
	}()

	err = cmd.Wait()
	readerWg.Wait() // ensure all PTY output is drained before returning

	ws.mu.Lock()
	ws.cmd = nil
	ws.mu.Unlock()
	return err
}

// runCmd kept for non-wipe use (detection, mount, eject)
func runCmd(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// ── Main ─────────────────────────────────────────────────────────────────────

func cleanupStaleMounts() {
	// Clean up mounts left by prior crashed sessions.
	// Only match mount points under the system temp dir with the "usbwipe-" prefix
	// created by os.MkdirTemp("", "usbwipe-").
	tmpPrefix := os.TempDir() + "/usbwipe-"
	mounts := parseProcMounts()
	for dev, mps := range mounts {
		if !strings.HasPrefix(dev, "/dev/") {
			continue
		}
		for _, mp := range mps {
			if strings.HasPrefix(mp, tmpPrefix) {
				logv("cleaning up stale mount: %s on %s", dev, mp)
				exec.Command("umount", mp).Run()
				os.Remove(mp)
			}
		}
	}
	// Also clean up any leftover unmounted temp dirs
	entries, _ := filepath.Glob(os.TempDir() + "/usbwipe-*")
	for _, e := range entries {
		os.Remove(e) // only succeeds if empty (already unmounted)
	}
}

func main() {
	flag.BoolVar(&verbose, "v", false, "verbose diagnostic output")
	flag.Parse()

	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "Error: must be run as root")
		os.Exit(1)
	}

	// Clean up stale browse mounts from prior crashed sessions
	cleanupStaleMounts()

	devices, err := detectUSBDevices()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error detecting USB devices: %v\n", err)
		os.Exit(1)
	}
	if len(devices) == 0 {
		fmt.Println("No removable USB drives found.")
		os.Exit(0)
	}

	// Ignore SIGINT so ctrl+c is handled by the TUI, not the OS signal handler
	signal.Ignore(syscall.SIGINT)

	m := newModel(devices)
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
