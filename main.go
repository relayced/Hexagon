package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	ScriptVersion = "1.4.2"
	VersionURL    = "https://raw.githubusercontent.com/relayced/Hexagon/main/version.txt"
	AuthAPIURL    = "https://nefarious-auth.johnlhoydbugayyy.workers.dev"
	LogFileName   = "farming_log.txt"
)

// ANSI Color Palette
const (
	NC    = "\033[0m"
	Bold  = "\033[1m"
	Dim   = "\033[2m"
	White = "\033[1;37m"
	Gray  = "\033[38;5;244m"
	Dark  = "\033[38;5;238m"
	Cyan  = "\033[38;5;75m"
	Green = "\033[38;5;78m"
	Amber = "\033[38;5;214m"
	Red   = "\033[38;5;203m"
)

var allPackages = []string{
	"com.roblox.clienb",
	"com.roblox.clienc",
	"com.roblox.cliend",
	"com.roblox.cliene",
	"com.roblox.clienf",
	"com.roblox.clieng",
}

// Global runtime configurations
var (
	licenseKey      string
	licenseDuration string
	myHWID          string
	discordWebhook  string
	gameName        string
	gameURL         string
	cloneCount      int
	enableRejoin    bool
	activePackages  []string

	serverPlaceID  string
	serverGameName string

	inputChan = make(chan string, 16)

	consoleMu     sync.Mutex
	activeSpinner *spinnerState

	logMu sync.Mutex

	globalRecoveryLock sync.Mutex
	recoveringClones   = make(map[string]bool)
	recoveringMu       sync.Mutex

	networkMu     sync.RWMutex
	networkOnline = true

	playerPkgMap        = make(map[string]string)
	playerPkgMu         sync.RWMutex
	recentlyLaunchedPkg string
	recentlyLaunchedMu  sync.Mutex
)

// ============================================================================
// ANIMATED SPINNER & THREAD-SAFE CONSOLE SUBSYSTEM
// ============================================================================

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

type spinnerState struct {
	label     string
	remaining int // seconds remaining, or -1 if indefinite
	total     int
	frameIdx  int
	done      bool
}

func (s *spinnerState) renderUnsafe() {
	if s.done {
		return
	}
	frame := spinnerFrames[s.frameIdx%len(spinnerFrames)]
	if s.remaining >= 0 {
		fmt.Printf("\r\033[K  %s%s%s %-38s %s[%2ds]%s", Cyan, frame, NC, s.label, White, s.remaining, NC)
	} else {
		fmt.Printf("\r\033[K  %s%s%s %s", Cyan, frame, NC, s.label)
	}
}

// safeLog cleanly logs a message, erasing any active spinner frame, printing the log with a newline,
// and redrawing the active spinner beneath it. This prevents any log interleaving or collisions.
func safeLog(format string, a ...interface{}) {
	consoleMu.Lock()
	defer consoleMu.Unlock()

	if activeSpinner != nil && !activeSpinner.done {
		fmt.Print("\r\033[K")
	}

	msg := fmt.Sprintf(format, a...)
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	fmt.Print(msg)

	if activeSpinner != nil && !activeSpinner.done {
		activeSpinner.renderUnsafe()
	}
}

// runAnimatedCountdown runs a synchronized countdown with a smooth braille spinner.
func runAnimatedCountdown(label string, totalSeconds int, doneTag string, doneMsg string) {
	s := &spinnerState{
		label:     label,
		remaining: totalSeconds,
		total:     totalSeconds,
	}

	consoleMu.Lock()
	activeSpinner = s
	s.renderUnsafe()
	consoleMu.Unlock()

	frameTicker := time.NewTicker(80 * time.Millisecond)
	defer frameTicker.Stop()

	secTicker := time.NewTicker(1 * time.Second)
	defer secTicker.Stop()

	for s.remaining > 0 {
		select {
		case <-frameTicker.C:
			consoleMu.Lock()
			if activeSpinner == s && !s.done {
				s.frameIdx++
				s.renderUnsafe()
			}
			consoleMu.Unlock()
		case <-secTicker.C:
			consoleMu.Lock()
			if activeSpinner == s && !s.done {
				s.remaining--
				if s.remaining >= 0 {
					s.renderUnsafe()
				}
			}
			consoleMu.Unlock()
		}
	}

	consoleMu.Lock()
	s.done = true
	if activeSpinner == s {
		activeSpinner = nil
	}
	fmt.Print("\r\033[K")
	if doneTag != "" {
		fmt.Printf("  %s[%s]%s %s\n", Green, doneTag, NC, doneMsg)
	}
	consoleMu.Unlock()
}

// runAnimatedTask displays a spinner while an asynchronous task executes.
func runAnimatedTask(label string, task func() error) error {
	stopChan := make(chan struct{})
	s := &spinnerState{
		label:     label,
		remaining: -1,
	}

	consoleMu.Lock()
	activeSpinner = s
	s.renderUnsafe()
	consoleMu.Unlock()

	go func() {
		ticker := time.NewTicker(80 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopChan:
				return
			case <-ticker.C:
				consoleMu.Lock()
				if activeSpinner == s && !s.done {
					s.frameIdx++
					s.renderUnsafe()
				}
				consoleMu.Unlock()
			}
		}
	}()

	err := task()

	consoleMu.Lock()
	close(stopChan)
	s.done = true
	if activeSpinner == s {
		activeSpinner = nil
	}
	fmt.Print("\r\033[K")
	consoleMu.Unlock()

	return err
}

func getCloneDisplayName(pkg string) string {
	for i, p := range activePackages {
		if p == pkg {
			return fmt.Sprintf("Clone %d", i+1)
		}
	}
	for i, p := range allPackages {
		if p == pkg {
			return fmt.Sprintf("Clone %d", i+1)
		}
	}
	if strings.Contains(pkg, "Clone") {
		return pkg
	}
	return "Clone"
}

func cleanSentinelLogLine(line string) string {
	cleaned := line
	for i, p := range allPackages {
		cleaned = strings.ReplaceAll(cleaned, p, fmt.Sprintf("Clone %d", i+1))
	}
	return cleaned
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// ============================================================================
// SYSTEM RAM & PACKAGE INSTALLATION DETECTION
// ============================================================================

type SystemMemory struct {
	TotalMB     int
	AvailableMB int
	TotalGB     float64
	AvailableGB float64
}

func getSystemMemory() SystemMemory {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return SystemMemory{TotalMB: 0, AvailableMB: 0, TotalGB: 0, AvailableGB: 0}
	}

	var totalKB, availKB, freeKB int
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			key := strings.TrimSuffix(fields[0], ":")
			val, _ := strconv.Atoi(fields[1])
			switch key {
			case "MemTotal":
				totalKB = val
			case "MemAvailable":
				availKB = val
			case "MemFree":
				freeKB = val
			}
		}
	}

	if availKB == 0 {
		availKB = freeKB
	}

	totalMB := totalKB / 1024
	availMB := availKB / 1024

	return SystemMemory{
		TotalMB:     totalMB,
		AvailableMB: availMB,
		TotalGB:     float64(totalMB) / 1024.0,
		AvailableGB: float64(availMB) / 1024.0,
	}
}

func getRecommendedClones(mem SystemMemory) int {
	if mem.TotalMB == 0 {
		return 2 // fallback default
	}

	total := mem.TotalMB
	if total < 2800 { // Under 3GB RAM (e.g. 2GB device)
		return 1
	} else if total < 4600 { // ~3GB - 4GB RAM
		return 2
	} else if total < 6800 { // ~5GB - 6GB RAM
		return 3
	} else if total < 9000 { // ~7GB - 8GB RAM
		return 4
	} else if total < 13000 { // ~10GB - 12GB RAM
		return 5
	}
	// 12GB+ RAM
	return 6
}

func isPackageInstalled(pkg string) bool {
	// 1. Try pm path
	cmd := exec.Command("pm", "path", pkg)
	out, err := cmd.Output()
	if err == nil && strings.Contains(string(out), "package:") {
		return true
	}

	// 2. Try /system/bin/pm path directly
	cmd2 := exec.Command("/system/bin/pm", "path", pkg)
	out2, err2 := cmd2.Output()
	if err2 == nil && strings.Contains(string(out2), "package:") {
		return true
	}

	// 3. Try pm list packages
	cmd3 := exec.Command("pm", "list", "packages", pkg)
	out3, err3 := cmd3.Output()
	if err3 == nil {
		for _, l := range strings.Split(string(out3), "\n") {
			if strings.TrimSpace(l) == "package:"+pkg {
				return true
			}
		}
	}

	// 4. Try /system/bin/pm list packages
	cmd4 := exec.Command("/system/bin/pm", "list", "packages", pkg)
	out4, err4 := cmd4.Output()
	if err4 == nil {
		for _, l := range strings.Split(string(out4), "\n") {
			if strings.TrimSpace(l) == "package:"+pkg {
				return true
			}
		}
	}

	// 5. Try filesystem checks for app data
	if fi, err := os.Stat("/data/data/" + pkg); err == nil && fi.IsDir() {
		return true
	}
	if fi, err := os.Stat("/sdcard/Android/data/" + pkg); err == nil && fi.IsDir() {
		return true
	}
	if fi, err := os.Stat("/storage/emulated/0/Android/data/" + pkg); err == nil && fi.IsDir() {
		return true
	}

	return false
}

func checkInstalledClones(count int) ([]string, bool) {
	// Determine if running on Android
	isAndroid := false
	if _, err := exec.LookPath("pm"); err == nil {
		isAndroid = true
	} else if _, err := os.Stat("/system/bin/pm"); err == nil {
		isAndroid = true
	} else if _, err := os.Stat("/system/build.prop"); err == nil {
		isAndroid = true
	}

	if !isAndroid {
		return nil, true
	}

	var missing []string
	for i := 0; i < count; i++ {
		pkg := allPackages[i]
		if !isPackageInstalled(pkg) {
			missing = append(missing, fmt.Sprintf("Clone %d", i+1))
		}
	}

	if len(missing) > 0 {
		return missing, false
	}
	return nil, true
}

// ============================================================================
// INPUT & LOGGING HELPERS
// ============================================================================

func initInputReader() {
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			inputChan <- scanner.Text()
		}
	}()
}

func readLine() string {
	return <-inputChan
}

func readLineWithTimeout(timeout time.Duration) (string, bool) {
	select {
	case line := <-inputChan:
		return line, true
	case <-time.After(timeout):
		return "", false
	}
}

func drainInput() {
	for {
		select {
		case <-inputChan:
		default:
			return
		}
	}
}

// Auth Response Structs
type AuthRequest struct {
	Key  string `json:"key"`
	HWID string `json:"hwid"`
}

type AuthResponse struct {
	Success         bool   `json:"success"`
	Error           string `json:"error"`
	Message         string `json:"message"`
	Tier            string `json:"tier"`
	BoundHWID       string `json:"bound_hwid"`
	DefaultPlaceID  string `json:"default_place_id"`
	DefaultGameName string `json:"default_game_name"`
}

// Discord Webhook Payload
type DiscordWebhookPayload struct {
	Embeds []DiscordEmbed `json:"embeds"`
}

type DiscordEmbed struct {
	Title       string        `json:"title"`
	Description string        `json:"description"`
	Color       int           `json:"color"`
	Footer      DiscordFooter `json:"footer"`
}

type DiscordFooter struct {
	Text string `json:"text"`
}

func getHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	if home == "" {
		home = "/data/data/com.termux/files/home"
	}
	return home
}

func writeLog(tag, msg string) {
	logMu.Lock()
	defer logMu.Unlock()

	cleanedMsg := cleanSentinelLogLine(msg)
	entry := fmt.Sprintf("[%s] [%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), tag, cleanedMsg)
	f, err := os.OpenFile(LogFileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		defer f.Close()
		_, _ = f.WriteString(entry)
	}
}

func sendWebhook(title, message string, color int) {
	if discordWebhook == "" {
		return
	}

	cleanedMsg := cleanSentinelLogLine(message)

	payload := DiscordWebhookPayload{
		Embeds: []DiscordEmbed{
			{
				Title:       title,
				Description: cleanedMsg,
				Color:       color,
				Footer: DiscordFooter{
					Text: "Nefarious Hub Sentinel",
				},
			},
		},
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return
	}

	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		req, err := http.NewRequest("POST", discordWebhook, bytes.NewBuffer(data))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err == nil && resp != nil {
				_ = resp.Body.Close()
			}
		}
	}()
}

func getDeviceHWID() string {
	readCmd := func(name string, args ...string) string {
		out, err := exec.Command(name, args...).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}

	aid := readCmd("settings", "get", "secure", "android_id")
	model := readCmd("getprop", "ro.product.model")
	build := readCmd("getprop", "ro.build.id")
	serial := readCmd("getprop", "ro.serialno")

	raw := fmt.Sprintf("%s_%s_%s_%s", aid, model, build, serial)
	if aid == "" && model == "" {
		unameOut, _ := exec.Command("uname", "-a").Output()
		whoamiOut, _ := exec.Command("whoami").Output()
		raw = fmt.Sprintf("%s_%s", strings.TrimSpace(string(unameOut)), strings.TrimSpace(string(whoamiOut)))
	}

	hash := sha256.Sum256([]byte(raw))
	hwidHex := fmt.Sprintf("%x", hash)
	var finalHWID string
	if len(hwidHex) >= 16 {
		finalHWID = strings.ToUpper(hwidHex[:16])
	} else {
		finalHWID = strings.ToUpper(hwidHex)
	}
	_ = os.WriteFile("/sdcard/nefarious_hwid.txt", []byte(finalHWID), 0644)
	_ = os.WriteFile("/sdcard/Delta/nefarious_hwid.txt", []byte(finalHWID), 0644)

	// Clean up and remove any legacy nefarious_client.lua from Delta autoexec paths
	cleanTargets := []string{
		"/sdcard/Delta/autoexec/nefarious_client.lua",
		"/sdcard/nefarious_client.lua",
		"/storage/emulated/0/Delta/autoexec/nefarious_client.lua",
	}
	for _, pkg := range allPackages {
		cleanTargets = append(cleanTargets, fmt.Sprintf("/sdcard/Android/data/%s/files/Delta/autoexec/nefarious_client.lua", pkg))
	}
	for _, target := range cleanTargets {
		_ = os.Remove(target)
	}

	return finalHWID
}

func drawBanner() {
	fmt.Print("\033[H\033[2J")
	fmt.Println()
	fmt.Printf("%s┌──────────────────────────────────────────┐%s\n", Gray, NC)
	fmt.Printf("%s│%s  %s%s%-24s%s %s%13s%s  %s│%s\n", Gray, NC, Bold, White, "NEFARIOUS HUB", NC, Cyan, "v"+ScriptVersion, NC, Gray, NC)
	fmt.Printf("%s│%s  %s%-38s%s  %s│%s\n", Gray, NC, Dim, "Sentinel & Multi-Instance Recovery", NC, Gray, NC)
	if licenseKey != "" {
		fmt.Printf("%s├──────────────────────────────────────────┤%s\n", Gray, NC)
		fmt.Printf("%s│%s  %s%-8s%s %s%-29s%s  %s│%s\n", Gray, NC, Gray, "Key  :", NC, White, truncate(licenseKey, 29), NC, Gray, NC)
		fmt.Printf("%s│%s  %s%-8s%s %s%-29s%s  %s│%s\n", Gray, NC, Gray, "Tier :", NC, Green, truncate(licenseDuration, 29), NC, Gray, NC)
		fmt.Printf("%s│%s  %s%-8s%s %s%-29s%s  %s│%s\n", Gray, NC, Gray, "HWID :", NC, Cyan, truncate(myHWID, 29), NC, Gray, NC)
	}
	fmt.Printf("%s├──────────────────────────────────────────┤%s\n", Gray, NC)
	fmt.Printf("%s│%s  %s%-8s%s %s%-29s%s  %s│%s\n", Gray, NC, Gray, "Devs :", NC, White, "@NightWitch & @Jep", NC, Gray, NC)
	fmt.Printf("%s└──────────────────────────────────────────┘%s\n", Gray, NC)
	fmt.Println()
}

func drawAlertCard(cardType, title, line1, line2, line3 string) {
	borderColor := Gray
	switch cardType {
	case "ERROR":
		borderColor = Red
	case "WARN":
		borderColor = Amber
	case "SUCCESS":
		borderColor = Green
	}

	fmt.Println()
	fmt.Printf("%s┌──────────────────────────────────────────┐%s\n", borderColor, NC)
	fmt.Printf("%s│%s  %s%-38s%s  %s│%s\n", borderColor, NC, Bold, truncate(title, 38), NC, borderColor, NC)
	fmt.Printf("%s├──────────────────────────────────────────┤%s\n", borderColor, NC)
	if line1 != "" {
		fmt.Printf("%s│%s  %-38s  %s│%s\n", borderColor, NC, truncate(line1, 38), borderColor, NC)
	}
	if line2 != "" {
		fmt.Printf("%s│%s  %-38s  %s│%s\n", borderColor, NC, truncate(line2, 38), borderColor, NC)
	}
	if line3 != "" {
		fmt.Printf("%s│%s  %-38s  %s│%s\n", borderColor, NC, truncate(line3, 38), borderColor, NC)
	}
	fmt.Printf("%s└──────────────────────────────────────────┘%s\n", borderColor, NC)
	fmt.Println()
}

func drawSummaryCard() {
	fmt.Printf("%s┌──────────────────────────────────────────┐%s\n", Gray, NC)
	fmt.Printf("%s│%s  %s%s%-38s%s  %s│%s\n", Gray, NC, Bold, White, "SESSION PRE-FLIGHT", NC, Gray, NC)
	fmt.Printf("%s├──────────────────────────────────────────┤%s\n", Gray, NC)
	fmt.Printf("%s│%s  %s%-11s%s %s%-26s%s  %s│%s\n", Gray, NC, Gray, "Experience:", NC, White, truncate(gameName, 26), NC, Gray, NC)
	if strings.Contains(gameURL, "share?") || strings.Contains(gameURL, "privateServer") || strings.Contains(gameName, "[VIP]") {
		fmt.Printf("%s│%s  %s%-11s%s %s%-26s%s  %s│%s\n", Gray, NC, Gray, "Type      :", NC, Green, "Private Server (VIP)", NC, Gray, NC)
	} else {
		fmt.Printf("%s│%s  %s%-11s%s %s%-26s%s  %s│%s\n", Gray, NC, Gray, "Type      :", NC, White, "Public Server", NC, Gray, NC)
	}
	fmt.Printf("%s│%s  %s%-11s%s %s%-26s%s  %s│%s\n", Gray, NC, Gray, "Instances :", NC, White, fmt.Sprintf("%d Clone%s", cloneCount, plural(cloneCount)), NC, Gray, NC)
	if enableRejoin {
		fmt.Printf("%s│%s  %s%-11s%s %s%-26s%s  %s│%s\n", Gray, NC, Gray, "Sentinel  :", NC, Green, "ACTIVE", NC, Gray, NC)
	} else {
		fmt.Printf("%s│%s  %s%-11s%s %s%-26s%s  %s│%s\n", Gray, NC, Gray, "Sentinel  :", NC, Dark, "DISABLED (One-time)", NC, Gray, NC)
	}
	if discordWebhook != "" {
		fmt.Printf("%s│%s  %s%-11s%s %s%-26s%s  %s│%s\n", Gray, NC, Gray, "Discord   :", NC, Green, "ENABLED", NC, Gray, NC)
	} else {
		fmt.Printf("%s│%s  %s%-11s%s %s%-26s%s  %s│%s\n", Gray, NC, Gray, "Discord   :", NC, Dark, "DISABLED", NC, Gray, NC)
	}
	fmt.Printf("%s└──────────────────────────────────────────┘%s\n", Gray, NC)
}

func truncate(s string, maxLen int) string {
	if len(s) > maxLen {
		if maxLen > 3 {
			return s[:maxLen-3] + "..."
		}
		return s[:maxLen]
	}
	return s
}

func checkUpdates() {
	var latest string
	var reqErr error

	_ = runAnimatedTask("Checking for updates...", func() error {
		client := &http.Client{Timeout: 5 * time.Second}
		reqURL := fmt.Sprintf("%s?_t=%d", VersionURL, time.Now().Unix())
		resp, err := client.Get(reqURL)
		if err != nil {
			resp, err = client.Get(VersionURL)
		}
		if err != nil {
			reqErr = err
			return err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		latest = strings.TrimSpace(string(body))
		return nil
	})

	if latest != "" && latest != ScriptVersion {
		safeLog("  %s[UPDATE REQUIRED]%s Newer version v%s available (installed: v%s)", Amber, NC, latest, ScriptVersion)
		drawAlertCard("WARN", "[!] UPDATE REQUIRED",
			"A newer version is available.",
			fmt.Sprintf("Installed : v%s  |  Latest : v%s", ScriptVersion, latest),
			"Please update from GitHub or Discord.")
		os.Exit(1)
	}

	if reqErr != nil {
		safeLog("  %s[INFO]%s Update check skipped (offline/cached v%s)", Gray, NC, ScriptVersion)
	} else {
		safeLog("  %s[OK]%s Version v%s up to date", Green, NC, ScriptVersion)
	}
}

func verifyLicense() {
	licenseKey = "OPEN-SOURCE"
	licenseDuration = "Lifetime"
	serverPlaceID = "107778070777162"
	serverGameName = "Steal An Egg"
	safeLog("  %s[LICENSE]%s Open-source release - All features unlocked", Green, NC)
}

func configureConcurrency() {
	drainInput()
	mem := getSystemMemory()
	rec := getRecommendedClones(mem)

	for {
		drawBanner()
		fmt.Printf("%s── %s%s1. Instance Concurrency%s %s─────────────────%s\n", Gray, White, Bold, NC, Gray, NC)
		if mem.TotalMB > 0 {
			fmt.Printf("%sDevice RAM  :%s %s%.1f GB Total%s (%s%.1f GB Available%s)\n", Gray, NC, White, mem.TotalGB, NC, Cyan, mem.AvailableGB, NC)
			fmt.Printf("%sRecommended :%s %s%d Clone%s%s %s(optimized for your device)%s\n\n", Gray, NC, Green, rec, plural(rec), NC, Dim, NC)
		} else {
			fmt.Printf("%sRecommended :%s %s2 Clones%s\n\n", Gray, NC, Green, NC)
		}

		fmt.Printf("%sSelect number of Roblox clones to run:%s\n\n", Gray, NC)

		for i := 1; i <= 6; i++ {
			var note string
			var color string = White
			if i == rec {
				note = fmt.Sprintf("%s(Recommended for your RAM)%s", Green, NC)
				color = Green
			} else if i < rec {
				if i == 1 {
					note = fmt.Sprintf("%s(Solo instance - Lightest memory)%s", Dim, NC)
				} else {
					note = fmt.Sprintf("%s(Safe & lightweight)%s", Dim, NC)
				}
			} else { // i > rec
				diff := i - rec
				if diff == 1 {
					note = fmt.Sprintf("%s(Above recommended - Moderate RAM pressure)%s", Amber, NC)
				} else if diff == 2 {
					note = fmt.Sprintf("%s(Above recommended - High risk of crash / OOM)%s", Amber, NC)
				} else {
					note = fmt.Sprintf("%s(Above recommended - Extreme RAM pressure)%s", Red, NC)
				}
			}

			fmt.Printf("  %s[%d]%s %d Clone%s  %s\n", color, i, NC, i, plural(i), note)
		}
		fmt.Println()

		fmt.Printf("%s› Clones [1-6] (default: %d): %s", White, rec, NC)
		input := strings.TrimSpace(readLine())
		selected := rec
		if input != "" {
			c, err := strconv.Atoi(input)
			if err != nil || c < 1 || c > 6 {
				fmt.Printf("%s[!] Invalid entry. Enter a number between 1 and 6.%s\n", Red, NC)
				time.Sleep(1 * time.Second)
				continue
			}
			selected = c
		}

		// If user selects above recommended, display note and confirmation
		if selected > rec {
			fmt.Println()
			fmt.Printf("%s[NOTE] %d clones is above the recommended limit (%d Clone%s) for your RAM.%s\n", Amber, selected, rec, plural(rec), NC)
			fmt.Printf("%s       Android Low Memory Killer (LMK) may force-close background clones.%s\n", Gray, NC)
			fmt.Printf("%s› Proceed with %d clones anyway? [y/N]: %s", White, selected, NC)
			conf := strings.ToLower(strings.TrimSpace(readLine()))
			if conf != "y" && conf != "yes" {
				continue
			}
		}

		// Verify installed packages before proceeding
		missing, ok := checkInstalledClones(selected)
		if !ok {
			fmt.Println()
			fmt.Printf("%s┌──────────────────────────────────────────┐%s\n", Red, NC)
			fmt.Printf("%s│%s  %s%-38s%s  %s│%s\n", Red, NC, Bold, "[!] CLONE APP NOT INSTALLED", NC, Red, NC)
			fmt.Printf("%s├──────────────────────────────────────────┤%s\n", Red, NC)
			fmt.Printf("%s│%s  The following clone app(s) are not     %s│%s\n", Red, NC, Red, NC)
			fmt.Printf("%s│%s  installed on this device:              %s│%s\n", Red, NC, Red, NC)
			for _, m := range missing {
				fmt.Printf("%s│%s    %s• %-34s%s %s│%s\n", Red, NC, Amber, truncate(m, 34), NC, Red, NC)
			}
			fmt.Printf("%s│%s                                          %s│%s\n", Red, NC, Red, NC)
			fmt.Printf("%s│%s  %sCannot proceed.%s Please install the     %s│%s\n", Red, NC, White, NC, Red, NC)
			if selected > 1 {
				fmt.Printf("%s│%s  missing APK(s) or select fewer clones. %s│%s\n", Red, NC, Red, NC)
			} else {
				fmt.Printf("%s│%s  missing APK before running Nefarious.  %s│%s\n", Red, NC, Red, NC)
			}
			fmt.Printf("%s└──────────────────────────────────────────┘%s\n", Red, NC)
			fmt.Printf("\n%sPress [ENTER] to choose another clone count...%s", White, NC)
			readLine()
			continue
		}

		cloneCount = selected
		activePackages = allPackages[:cloneCount]
		break
	}
}

func configureTargetExperience() {
	for {
		drawBanner()
		fmt.Printf("%s── %s%s2. Target Experience%s %s────────────────────%s\n", Gray, White, Bold, NC, Gray, NC)
		fmt.Printf("%sSelect the Roblox game to launch:%s\n\n", Gray, NC)
		fmt.Printf("  %s[1]%s Steal An Egg (Public Server)\n", White, NC)
		fmt.Printf("      %sID: 107778070777162%s\n\n", Dim, NC)
		fmt.Printf("  %s[2]%s Custom Game (Public Server)\n", White, NC)
		fmt.Printf("      %sPaste public game link or Place ID%s\n\n", Dim, NC)
		fmt.Printf("  %s[3]%s Private Server (VIP / Share Link)\n", White, NC)
		fmt.Printf("      %sPaste share link (e.g. https://www.roblox.com/share?code=...&type=Server)%s\n\n", Dim, NC)
		fmt.Printf("%s› Selection [1-3] (default: 1): %s", White, NC)

		choice := strings.TrimSpace(readLine())

		if choice == "" || choice == "1" {
			targetPlaceID := serverPlaceID
			if targetPlaceID == "" {
				targetPlaceID = "107778070777162"
			}
			targetName := serverGameName
			if targetName == "" {
				targetName = "Steal An Egg"
			}
			gameURL = "roblox://placeId=" + targetPlaceID
			gameName = targetName
			break
		} else if choice == "2" {
			goBack := false
			for {
				fmt.Println()
				fmt.Printf("%s── Custom Public Experience ───────────────%s\n", Gray, NC)
				fmt.Printf("%sPaste full game link, or type 'back':%s\n", Gray, NC)
				fmt.Printf("%s› URL / Place ID: %s", White, NC)

				link := strings.TrimSpace(readLine())

				if strings.ToLower(link) == "back" {
					goBack = true
					break
				}

				customID := ""
				rePlace := regexp.MustCompile(`[?&]placeId=([0-9]+)`)
				if m := rePlace.FindStringSubmatch(link); len(m) > 1 {
					customID = m[1]
				}
				if customID == "" {
					reGames := regexp.MustCompile(`/games/([0-9]+)`)
					if m := reGames.FindStringSubmatch(link); len(m) > 1 {
						customID = m[1]
					}
				}
				if customID == "" && regexp.MustCompile(`^[0-9]+$`).MatchString(link) {
					customID = link
				}

				if customID == "" {
					fmt.Printf("%s[!] Could not detect Place ID. Enter a valid game URL or numeric ID.%s\n", Red, NC)
					continue
				}

				cName := ""
				reName := regexp.MustCompile(`/games/[0-9]+/([^/?]*)`)
				if m := reName.FindStringSubmatch(link); len(m) > 1 && m[1] != "" {
					cName = strings.ReplaceAll(m[1], "-", " ")
				}
				if cName == "" {
					cName = "Game " + customID
				}

				fmt.Println()
				fmt.Printf("%sDetected Parameters:%s\n", Gray, NC)
				fmt.Printf("  %sType     :%s %sPublic Server%s\n", Gray, NC, White, NC)
				fmt.Printf("  %sPlace ID :%s %s%s%s\n", Gray, NC, White, customID, NC)
				fmt.Printf("  %sName     :%s %s%s%s\n\n", Gray, NC, White, cName, NC)
				fmt.Printf("%s› Confirm configuration? [Y/n]: %s", White, NC)

				confirm := strings.ToLower(strings.TrimSpace(readLine()))
				if confirm == "" || confirm == "y" || confirm == "yes" {
					gameURL = "roblox://placeId=" + customID
					gameName = cName
					return
				}
			}
			if goBack {
				continue
			}
		} else if choice == "3" {
			goBack := false
			psCache := filepath.Join(getHomeDir(), ".nefhub_ps_cache")
			cachedPSName := ""
			cachedPSURL := ""
			if data, err := os.ReadFile(psCache); err == nil {
				parts := strings.Split(strings.TrimSpace(string(data)), "|")
				if len(parts) >= 2 {
					cachedPSName = parts[0]
					cachedPSURL = parts[1]
				} else if len(parts) == 1 {
					cachedPSURL = parts[0]
				}
			}

			for {
				fmt.Println()
				fmt.Printf("%s── Private Server Configuration ───────────%s\n", Gray, NC)
				if cachedPSURL != "" {
					fmt.Printf("%sSaved Private Server Detected:%s\n", Gray, NC)
					if cachedPSName != "" {
						fmt.Printf("  Name: %s%s%s\n", White, cachedPSName, NC)
					}
					fmt.Printf("  URL : %s%s%s\n\n", Cyan, truncate(cachedPSURL, 38), NC)
					fmt.Printf("%sPress [ENTER] to use saved Private Server, or paste new / type 'back':%s\n", White, NC)
				} else {
					fmt.Printf("%sPaste Private Server share link or VIP URL, or type 'back':%s\n", Gray, NC)
					fmt.Printf("%sExample: https://www.roblox.com/share?code=91d4e592ecee2247aac83ee8c2b785f5&type=Server%s\n", Dim, NC)
				}
				fmt.Printf("%s› Private Server URL: %s", White, NC)

				link := strings.TrimSpace(readLine())
				link = strings.Trim(link, "\"'")

				if strings.ToLower(link) == "back" {
					goBack = true
					break
				}

				if link == "" && cachedPSURL != "" {
					gameURL = cachedPSURL
					if cachedPSName != "" {
						gameName = cachedPSName
					} else {
						gameName = "Private Server [VIP]"
					}
					return
				}

				if link == "" {
					fmt.Printf("%s[!] Private Server URL cannot be empty.%s\n", Red, NC)
					continue
				}

				if !strings.Contains(link, "roblox.com") && !strings.Contains(link, "roblox://") {
					fmt.Printf("%s[!] Invalid URL. Expected a Roblox link (e.g. https://www.roblox.com/share?code=...&type=Server)%s\n", Red, NC)
					continue
				}

				psName := ""
				fmt.Printf("%s› Enter Experience Name (optional, default: Private Server): %s", White, NC)
				nameInput := strings.TrimSpace(readLine())
				if nameInput != "" {
					if !strings.Contains(nameInput, "[VIP]") {
						psName = nameInput + " [VIP]"
					} else {
						psName = nameInput
					}
				} else {
					reName := regexp.MustCompile(`/games/[0-9]+/([^/?]*)`)
					if m := reName.FindStringSubmatch(link); len(m) > 1 && m[1] != "" {
						psName = strings.ReplaceAll(m[1], "-", " ") + " [VIP]"
					} else {
						psName = "Private Server [VIP]"
					}
				}

				fmt.Println()
				fmt.Printf("%sDetected Parameters:%s\n", Gray, NC)
				fmt.Printf("  %sType :%s %sPrivate Server (VIP Share Link)%s\n", Gray, NC, Green, NC)
				fmt.Printf("  %sName :%s %s%s%s\n", Gray, NC, White, psName, NC)
				fmt.Printf("  %sURL  :%s %s%s%s\n\n", Gray, NC, Cyan, truncate(link, 38), NC)
				fmt.Printf("%s› Confirm configuration? [Y/n]: %s", White, NC)

				confirm := strings.ToLower(strings.TrimSpace(readLine()))
				if confirm == "" || confirm == "y" || confirm == "yes" {
					gameURL = link
					gameName = psName
					_ = os.WriteFile(psCache, []byte(fmt.Sprintf("%s|%s", psName, link)), 0600)
					return
				}
			}
			if goBack {
				continue
			}
		} else {
			fmt.Printf("%s[!] Invalid choice. Enter 1, 2, or 3.%s\n", Red, NC)
			time.Sleep(1 * time.Second)
		}
	}
}

func configureSentinel() {
	drawBanner()
	fmt.Printf("%s── %s%s3. Sentinel Crash & In-Game Guard%s %s─────────%s\n", Gray, White, Bold, NC, Gray, NC)
	fmt.Printf("%sActively monitors if clones are dead, frozen,%s\n", Gray, NC)
	fmt.Printf("%sor disconnected to lobby, auto-rejoining.%s\n\n", Gray, NC)
	fmt.Printf("%s› Enable Sentinel monitor? [Y/n]: %s", White, NC)

	for {
		choice := strings.ToLower(strings.TrimSpace(readLine()))
		if choice == "" || choice == "y" || choice == "yes" {
			enableRejoin = true
			break
		} else if choice == "n" || choice == "no" {
			enableRejoin = false
			break
		}
		fmt.Printf("%sPlease enter Y or N: %s", Red, NC)
	}
}

func configureWebhook() {
	webhookCache := filepath.Join(getHomeDir(), ".nefhub_webhook")
	cachedWebhook := ""
	if data, err := os.ReadFile(webhookCache); err == nil {
		cachedWebhook = strings.TrimSpace(string(data))
	}

	fmt.Println()
	fmt.Printf("%s── %s%s4. Discord Notifications%s %s────────────────%s\n", Gray, White, Bold, NC, Gray, NC)
	fmt.Printf("%sSend crash, freeze, and recovery events%s\n", Gray, NC)
	fmt.Printf("%sdirectly to your Discord channel.%s\n\n", Gray, NC)

	validateURL := func(u string) bool {
		return strings.HasPrefix(u, "https://discord.com/api/webhooks/") ||
			strings.HasPrefix(u, "https://discordapp.com/api/webhooks/")
	}

	if cachedWebhook != "" {
		fmt.Printf("%s[INFO] Saved Webhook:%s\n", Gray, NC)
		fmt.Printf("       %s%s...%s\n\n", Cyan, truncate(cachedWebhook, 36), NC)
		fmt.Printf("%sPress [ENTER] to use saved webhook, or enter new / 'none':%s\n", White, NC)
		fmt.Printf("%s› %s", White, NC)
		input := strings.TrimSpace(readLine())

		if input == "" {
			discordWebhook = cachedWebhook
			fmt.Printf("%s[OK] Using saved Discord webhook.%s\n", Green, NC)
			time.Sleep(1 * time.Second)
		} else if strings.ToLower(input) == "none" || strings.ToLower(input) == "no" {
			discordWebhook = ""
			_ = os.Remove(webhookCache)
			fmt.Printf("%s[INFO] Discord webhook disabled.%s\n", Gray, NC)
			time.Sleep(1 * time.Second)
		} else {
			for {
				if validateURL(input) {
					discordWebhook = input
					_ = os.WriteFile(webhookCache, []byte(discordWebhook), 0600)
					fmt.Printf("%s[OK] Dispatching test notification to channel...%s\n", Green, NC)
					sendWebhook("Sentinel Connected", "Nefarious Hub monitoring connected.", 3066993)
					time.Sleep(2 * time.Second)
					break
				}
				fmt.Printf("%s[!] Invalid Discord URL. Must begin with https://discord.com/api/webhooks/%s\n", Red, NC)
				fmt.Printf("%s› Webhook URL: %s", White, NC)
				input = strings.TrimSpace(readLine())
			}
		}
	} else {
		fmt.Printf("%s› Configure Discord webhook? [y/N]: %s", White, NC)
		choice := strings.ToLower(strings.TrimSpace(readLine()))
		if choice == "y" || choice == "yes" {
			for {
				fmt.Printf("%s› Webhook URL: %s", White, NC)
				input := strings.TrimSpace(readLine())
				if validateURL(input) {
					discordWebhook = input
					_ = os.WriteFile(webhookCache, []byte(discordWebhook), 0600)
					fmt.Printf("%s[OK] Dispatching test notification to channel...%s\n", Green, NC)
					sendWebhook("Sentinel Connected", "Nefarious Hub monitoring connected.", 3066993)
					time.Sleep(2 * time.Second)
					break
				}
				fmt.Printf("%s[!] Invalid Discord URL. Must begin with https://discord.com/api/webhooks/%s\n", Red, NC)
			}
		} else {
			discordWebhook = ""
		}
	}
}

func launchInitialInstances() {
	writeLog("INIT", fmt.Sprintf("Session started with %d clones on %s", cloneCount, gameName))
	runAnimatedCountdown("Initializing runtime...", 3, "READY", "Runtime initialized")
	fmt.Println()

	fmt.Printf("%s── Launching Clones ────────────────────────%s\n", Gray, NC)
	for i, pkg := range activePackages {
		cloneNum := i + 1
		displayName := fmt.Sprintf("Clone %d", cloneNum)
		currTime := time.Now().Format("15:04:05")

		recentlyLaunchedMu.Lock()
		recentlyLaunchedPkg = pkg
		recentlyLaunchedMu.Unlock()
		_ = os.WriteFile("/sdcard/nefarious_active_clone.txt", []byte(fmt.Sprintf("%d|%s", cloneNum, pkg)), 0644)

		safeLog("[%s] %s[LAUNCH]%s   Launching %s%s%s...", currTime, Gray, NC, White, displayName, NC)
		_ = exec.Command("am", "start", "-a", "android.intent.action.MAIN", "-c", "android.intent.category.LAUNCHER", "-p", pkg).Run()

		runAnimatedCountdown(fmt.Sprintf("Initializing client engine (%s)...", displayName), 10, "READY", fmt.Sprintf("Client engine initialized (%s)", displayName))

		joinTime := time.Now().Format("15:04:05")
		safeLog("[%s] %s[JOIN]%s     Connecting %s%s%s to %s%s%s...", joinTime, Cyan, NC, White, displayName, NC, White, gameName, NC)
		_ = exec.Command("am", "start", "-a", "android.intent.action.VIEW", "-d", gameURL, "-p", pkg).Run()

		time.Sleep(5 * time.Second)
		altURL := gameURL
		if strings.HasPrefix(gameURL, "roblox://placeId=") {
			altURL = strings.Replace(gameURL, "roblox://placeId=", "roblox://experiences/start?placeId=", 1)
		} else if strings.HasPrefix(gameURL, "roblox://experiences/start?placeId=") {
			altURL = strings.Replace(gameURL, "roblox://experiences/start?placeId=", "roblox://placeId=", 1)
		}
		_ = exec.Command("am", "start", "-a", "android.intent.action.VIEW", "-d", altURL, "-p", pkg).Run()

		syncTime := time.Now().Format("15:04:05")
		safeLog("[%s] %s[OK]%s       %s%s%s synchronized with %s", syncTime, Green, NC, White, displayName, NC, gameName)
		writeLog("LAUNCH_OK", displayName)

		if i < len(activePackages)-1 {
			nextNum := cloneNum + 1
			runAnimatedCountdown(fmt.Sprintf("Cooling down before Clone %d...", nextNum), 20, "READY", "Cooldown complete")
		}
	}

	safeLog("%s────────────────────────────────────────────%s", Gray, NC)
	if enableRejoin {
		safeLog("%s[ACTIVE]     All %d clone%s synchronized.%s", Green, cloneCount, plural(cloneCount), NC)
		safeLog("%s[SENTINEL]   Active In-Game & Crash Sentinel running.%s", Cyan, NC)
		safeLog("%s[INFO]       Press Ctrl+C to terminate session.%s", Gray, NC)
	} else {
		safeLog("%s[DONE]       All %d clone%s launched. Exiting.%s", Green, cloneCount, plural(cloneCount), NC)
	}
	safeLog("%s────────────────────────────────────────────%s\n", Gray, NC)
}

func startSentinelMonitor() {
	time.Sleep(5 * time.Second)
	_ = exec.Command("logcat", "-c").Run()
	time.Sleep(1 * time.Second)

	filterRegex := regexp.MustCompile(`WIN DEATH|has died|am_kill|am_crash|ANR in|am_anr|Force stopping|Error Code: 273|Error Code: 277|Error Code:|Same account launched|Disconnected from`)
	anrRegex := regexp.MustCompile(`ANR in|am_anr`)
	disconnectRegex := regexp.MustCompile(`Error Code:|Same account launched|Disconnected from`)

	for {
		cmd := exec.Command("logcat")
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		if err := cmd.Start(); err != nil {
			time.Sleep(2 * time.Second)
			continue
		}

		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if !filterRegex.MatchString(line) {
				continue
			}

			// Ignore individual crash events if network is offline or full recovery is underway
			networkMu.RLock()
			netUp := networkOnline
			networkMu.RUnlock()
			if !netUp {
				continue
			}

			for i, pkg := range activePackages {
				matchesPkg := strings.Contains(line, pkg)
				if !matchesPkg && disconnectRegex.MatchString(line) {
					out, err := exec.Command("pidof", pkg).Output()
					if err == nil {
						pids := strings.Fields(string(out))
						for _, pid := range pids {
							if pid != "" && strings.Contains(line, pid) {
								matchesPkg = true
								break
							}
						}
					}
				}

				if matchesPkg {
					cloneNum := i + 1
					displayName := fmt.Sprintf("Clone %d", cloneNum)
					isANR := anrRegex.MatchString(line)

					recoveringMu.Lock()
					if recoveringClones[pkg] {
						recoveringMu.Unlock()
						continue
					}
					recoveringClones[pkg] = true
					recoveringMu.Unlock()

					go recoverClone(pkg, displayName, isANR)
				}
			}
		}
		_ = cmd.Wait()
		time.Sleep(2 * time.Second)
	}
}

func recoverClone(pkg, displayName string, isANR bool) {
	defer func() {
		recoveringMu.Lock()
		delete(recoveringClones, pkg)
		recoveringMu.Unlock()
	}()

	networkMu.RLock()
	netUp := networkOnline
	networkMu.RUnlock()
	if !netUp {
		return
	}

	eventTime := time.Now().Format("15:04:05")
	logTimestamp := time.Now().Format("2006-01-02 15:04:05")

	if isANR {
		safeLog("[%s] %s[ANR]%s      %s%s%s unresponsive (freeze). Rebooting...", eventTime, Amber, NC, White, displayName, NC)
		writeLog("ANR", fmt.Sprintf("%s unresponsive", displayName))
		sendWebhook("Freeze Detected (ANR)", fmt.Sprintf("**%s** stopped responding at %s. Rebooting...", displayName, logTimestamp), 15105570)
	} else {
		safeLog("[%s] %s[CRASH]%s    %s%s%s process terminated. Recovering...", eventTime, Red, NC, White, displayName, NC)
		writeLog("CRASH", fmt.Sprintf("%s terminated", displayName))
		sendWebhook("Crash Detected", fmt.Sprintf("**%s** crashed at %s. Relaunching...", displayName, logTimestamp), 15158332)
	}

	// Queue recovery so multiple instances don't spike CPU/RAM simultaneously
	globalRecoveryLock.Lock()
	defer globalRecoveryLock.Unlock()

	networkMu.RLock()
	netUp = networkOnline
	networkMu.RUnlock()
	if !netUp {
		return
	}

	// 1. Force-stop to clear stuck instances
	_ = exec.Command("am", "force-stop", pkg).Run()
	time.Sleep(1 * time.Second)

	recentlyLaunchedMu.Lock()
	recentlyLaunchedPkg = pkg
	recentlyLaunchedMu.Unlock()
	_ = os.WriteFile("/sdcard/nefarious_active_clone.txt", []byte(fmt.Sprintf("%s|%s", displayName, pkg)), 0644)

	// 2. Launch client engine
	_ = exec.Command("am", "start", "-a", "android.intent.action.MAIN", "-c", "android.intent.category.LAUNCHER", "-p", pkg).Run()

	// 3. Full 10-second client engine initialization animated countdown
	runAnimatedCountdown(fmt.Sprintf("Initializing client engine (%s)...", displayName), 10, "READY", fmt.Sprintf("Client engine initialized (%s)", displayName))

	// 4. First game connection intent
	joinTime := time.Now().Format("15:04:05")
	safeLog("[%s] %s[JOIN]%s     Connecting %s%s%s to %s%s%s...", joinTime, Cyan, NC, White, displayName, NC, White, gameName, NC)
	_ = exec.Command("am", "start", "-a", "android.intent.action.VIEW", "-d", gameURL, "-p", pkg).Run()

	// 5. 5-second interval before second intent pulse
	time.Sleep(5 * time.Second)

	// 6. Dual Intent Pulse (Guarantees connection without lobby stall)
	_ = exec.Command("am", "start", "-a", "android.intent.action.VIEW", "-d", gameURL, "-p", pkg).Run()

	reopenTime := time.Now().Format("15:04:05")
	safeLog("[%s] %s[OK]%s       %s%s%s synchronized with %s", reopenTime, Green, NC, White, displayName, NC, gameName)
	writeLog("RESTORE", fmt.Sprintf("%s recovered", displayName))

	// 7. Staggered stabilization countdown
	runAnimatedCountdown(fmt.Sprintf("Cooling down (%s)...", displayName), 20, "READY", fmt.Sprintf("Cooldown complete (%s)", displayName))

	stableTime := time.Now().Format("15:04:05")
	safeLog("[%s] %s[STABLE]%s   %s%s%s online. Monitoring resumed.", stableTime, Green, NC, White, displayName, NC)
	writeLog("STABLE", fmt.Sprintf("%s verified online", displayName))
	sendWebhook("Recovered", fmt.Sprintf("**%s** is back online as of %s.", displayName, time.Now().Format("2006-01-02 15:04:05")), 3066993)
}

func isInternetConnected() bool {
	conn, err := net.DialTimeout("tcp", "1.1.1.1:443", 2*time.Second)
	if err == nil {
		_ = conn.Close()
		return true
	}
	conn2, err2 := net.DialTimeout("tcp", "8.8.8.8:53", 2*time.Second)
	if err2 == nil {
		_ = conn2.Close()
		return true
	}
	return false
}

func waitForInternetAtStartup() {
	if isInternetConnected() {
		return
	}

	safeLog("%s[OFFLINE]%s No internet connection detected.", Red, NC)

	s := &spinnerState{
		label:     "Waiting for network connection...",
		remaining: -1,
	}

	consoleMu.Lock()
	activeSpinner = s
	s.renderUnsafe()
	consoleMu.Unlock()

	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			consoleMu.Lock()
			if activeSpinner == s && !s.done {
				s.frameIdx++
				s.renderUnsafe()
			}
			consoleMu.Unlock()
		default:
			if isInternetConnected() {
				consoleMu.Lock()
				s.done = true
				if activeSpinner == s {
					activeSpinner = nil
				}
				fmt.Print("\r\033[K")
				fmt.Printf("  %s[CONNECTED]%s Internet connection established.\n\n", Green, NC)
				consoleMu.Unlock()
				time.Sleep(1 * time.Second)
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
}

func getPublicIP() (string, error) {
	client := &http.Client{Timeout: 4 * time.Second}
	// 1. Direct Cloudflare IP query (fast, bypasses DNS resolution issues)
	resp, err := client.Get("https://1.1.1.1/cdn-cgi/trace")
	if err == nil {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		lines := strings.Split(string(body), "\n")
		for _, l := range lines {
			if strings.HasPrefix(l, "ip=") {
				ip := strings.TrimSpace(strings.TrimPrefix(l, "ip="))
				if ip != "" {
					return ip, nil
				}
			}
		}
	}

	// 2. Fallback to api.ipify.org
	resp2, err2 := client.Get("https://api.ipify.org")
	if err2 == nil {
		defer resp2.Body.Close()
		body, _ := io.ReadAll(resp2.Body)
		ip := strings.TrimSpace(string(body))
		if ip != "" {
			return ip, nil
		}
	}

	return "", fmt.Errorf("network unreachable")
}

func relaunchAllClones(reason string) {
	globalRecoveryLock.Lock()
	defer globalRecoveryLock.Unlock()

	recoveringMu.Lock()
	recoveringClones = make(map[string]bool)
	for _, pkg := range activePackages {
		recoveringClones[pkg] = true
	}
	recoveringMu.Unlock()

	defer func() {
		time.Sleep(2 * time.Second)
		recoveringMu.Lock()
		recoveringClones = make(map[string]bool)
		recoveringMu.Unlock()
	}()

	safeLog("\n%s────────────────────────────────────────────%s", Gray, NC)
	safeLog("%s[NET RESET]   Force-stopping & relaunching all clones (%s)...%s", Amber, reason, NC)
	writeLog("NET_RESET", reason)

	for _, pkg := range activePackages {
		_ = exec.Command("am", "force-stop", pkg).Run()
	}
	time.Sleep(2 * time.Second)

	for i, pkg := range activePackages {
		cloneNum := i + 1
		displayName := fmt.Sprintf("Clone %d", cloneNum)
		currTime := time.Now().Format("15:04:05")

		recentlyLaunchedMu.Lock()
		recentlyLaunchedPkg = pkg
		recentlyLaunchedMu.Unlock()

		safeLog("[%s] %s[LAUNCH]%s   Relaunching %s%s%s...", currTime, Gray, NC, White, displayName, NC)
		_ = exec.Command("am", "start", "-a", "android.intent.action.MAIN", "-c", "android.intent.category.LAUNCHER", "-p", pkg).Run()

		runAnimatedCountdown(fmt.Sprintf("Initializing client engine (%s)...", displayName), 10, "READY", fmt.Sprintf("Client engine initialized (%s)", displayName))

		joinTime := time.Now().Format("15:04:05")
		safeLog("[%s] %s[JOIN]%s     Connecting %s%s%s to %s%s%s...", joinTime, Cyan, NC, White, displayName, NC, White, gameName, NC)
		_ = exec.Command("am", "start", "-a", "android.intent.action.VIEW", "-d", gameURL, "-p", pkg).Run()

		time.Sleep(5 * time.Second)
		altURL := gameURL
		if strings.HasPrefix(gameURL, "roblox://placeId=") {
			altURL = strings.Replace(gameURL, "roblox://placeId=", "roblox://experiences/start?placeId=", 1)
		} else if strings.HasPrefix(gameURL, "roblox://experiences/start?placeId=") {
			altURL = strings.Replace(gameURL, "roblox://experiences/start?placeId=", "roblox://placeId=", 1)
		}
		_ = exec.Command("am", "start", "-a", "android.intent.action.VIEW", "-d", altURL, "-p", pkg).Run()

		syncTime := time.Now().Format("15:04:05")
		safeLog("[%s] %s[OK]%s       %s%s%s synchronized with %s", syncTime, Green, NC, White, displayName, NC, gameName)
		writeLog("NET_RELAUNCH_OK", displayName)

		if i < len(activePackages)-1 {
			nextNum := cloneNum + 1
			runAnimatedCountdown(fmt.Sprintf("Cooling down before Clone %d...", nextNum), 20, "READY", "Cooldown complete")
		}
	}

	safeLog("%s────────────────────────────────────────────%s", Gray, NC)
	safeLog("%s[ACTIVE]     All %d clone%s recovered and synchronized.%s", Green, cloneCount, plural(cloneCount), NC)
	safeLog("%s────────────────────────────────────────────%s\n", Gray, NC)
	sendWebhook("All Clones Restored", fmt.Sprintf("All %d Roblox clones successfully recovered after %s.", cloneCount, reason), 3066993)
}

func startNetworkMonitor() {
	var lastIP string
	wasOnline := isInternetConnected()

	if wasOnline {
		if ip, err := getPublicIP(); err == nil {
			lastIP = ip
		}
	}

	ticker := time.NewTicker(4 * time.Second)
	defer ticker.Stop()

	offlineTick := 0

	for range ticker.C {
		connected := isInternetConnected()
		var currentIP string
		if connected {
			if ip, err := getPublicIP(); err == nil {
				currentIP = ip
			} else {
				currentIP = lastIP
			}
		}

		isOnline := (connected && currentIP != "")

		// Case 1: Internet connection lost
		if wasOnline && !isOnline {
			wasOnline = false
			networkMu.Lock()
			networkOnline = false
			networkMu.Unlock()

			nowTime := time.Now().Format("15:04:05")
			safeLog("\n[%s] %s[NET LOST]%s   Internet connection lost! Force-stopping all clones...", nowTime, Red, NC)
			writeLog("NET_DISCONNECT", "Internet connection lost. Force-stopping all clones.")
			sendWebhook("Network Lost", "Internet connection lost. Force-stopping all Roblox clones to prevent freeze.", 15158332)

			globalRecoveryLock.Lock()
			for _, pkg := range activePackages {
				_ = exec.Command("am", "force-stop", pkg).Run()
			}
			globalRecoveryLock.Unlock()
			offlineTick = 0
			continue
		}

		// Periodic status while offline
		if !isOnline {
			offlineTick++
			if offlineTick%6 == 0 {
				currTime := time.Now().Format("15:04:05")
				safeLog("[%s] %s[OFFLINE]%s    No internet connection. Waiting for network...", currTime, Gray, NC)
			}
			continue
		}

		// Case 2: Internet reconnected after outage
		if !wasOnline && isOnline {
			wasOnline = true
			networkMu.Lock()
			networkOnline = true
			networkMu.Unlock()

			nowTime := time.Now().Format("15:04:05")
			safeLog("\n[%s] %s[NET RESTORED]%s Internet reconnected (IP: %s%s%s). Relaunching all clones...", nowTime, Green, NC, Cyan, currentIP, NC)
			writeLog("NET_RECONNECT", fmt.Sprintf("Internet reconnected (IP: %s). Relaunching all clones.", currentIP))
			sendWebhook("Network Restored", fmt.Sprintf("Internet reconnected with IP: `%s`. Relaunching all clones...", currentIP), 3066993)

			lastIP = currentIP
			relaunchAllClones("Network Reconnected")
			continue
		}

		// Case 3: Public IP changed while online (e.g. mobile IP rotated, proxy switched, VPN reconnected)
		if isOnline && lastIP != "" && currentIP != lastIP {
			nowTime := time.Now().Format("15:04:05")
			safeLog("\n[%s] %s[IP CHANGED]%s IP switch detected: %s%s%s ➔ %s%s%s. Force-stopping and relaunching...", nowTime, Amber, NC, Gray, lastIP, NC, Cyan, currentIP, NC)
			writeLog("IP_CHANGE", fmt.Sprintf("IP changed from %s to %s. Relaunching all clones.", lastIP, currentIP))
			sendWebhook("IP Change Detected", fmt.Sprintf("Device IP changed:\n**Old IP:** `%s`\n**New IP:** `%s`\nForce-stopping and relaunching all clones...", lastIP, currentIP), 15105570)

			lastIP = currentIP
			relaunchAllClones(fmt.Sprintf("IP Switched to %s", currentIP))
			continue
		}

		if isOnline && lastIP == "" {
			lastIP = currentIP
		}
	}
}

func startLocalBridgeServer() {
	mux := http.NewServeMux()

	// 1. Handshake endpoint
	mux.HandleFunc("/handshake", func(w http.ResponseWriter, r *http.Request) {
		player := strings.TrimSpace(r.URL.Query().Get("player"))

		recentlyLaunchedMu.Lock()
		assignedPkg := recentlyLaunchedPkg
		recentlyLaunchedMu.Unlock()

		if assignedPkg == "" && len(activePackages) > 0 {
			assignedPkg = activePackages[0]
		}

		cloneIdx := 1
		for i, p := range activePackages {
			if p == assignedPkg {
				cloneIdx = i + 1
				break
			}
		}

		if player != "" {
			playerPkgMu.Lock()
			playerPkgMap[player] = assignedPkg
			playerPkgMu.Unlock()
		}

		currTime := time.Now().Format("15:04:05")
		safeLog("[%s] %s[CONNECT]%s   Clone %d bound to player: %s%s%s",
			currTime, Green, NC, cloneIdx, White, player, NC)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "ok",
			"pkg":    assignedPkg,
			"clone":  cloneIdx,
		})
	})

	// Legacy /register fallback
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		player := strings.TrimSpace(r.URL.Query().Get("player"))
		if player != "" {
			recentlyLaunchedMu.Lock()
			assignedPkg := recentlyLaunchedPkg
			recentlyLaunchedMu.Unlock()

			if assignedPkg == "" && len(activePackages) > 0 {
				assignedPkg = activePackages[0]
			}

			playerPkgMu.Lock()
			playerPkgMap[player] = assignedPkg
			playerPkgMu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	// 2. Heartbeat endpoint
	mux.HandleFunc("/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("PONG"))
	})

	// 3. Report endpoint
	mux.HandleFunc("/report", func(w http.ResponseWriter, r *http.Request) {
		pkgParam := strings.TrimSpace(r.URL.Query().Get("pkg"))
		player := strings.TrimSpace(r.URL.Query().Get("player"))
		reason := strings.TrimSpace(r.URL.Query().Get("reason"))
		detail := strings.TrimSpace(r.URL.Query().Get("detail"))

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("RECEIVED"))

		if reason == "" {
			return
		}

		targetPkg := pkgParam
		if targetPkg == "" && player != "" {
			playerPkgMu.RLock()
			targetPkg = playerPkgMap[player]
			playerPkgMu.RUnlock()
		}
		if targetPkg == "" {
			recentlyLaunchedMu.Lock()
			targetPkg = recentlyLaunchedPkg
			recentlyLaunchedMu.Unlock()
		}
		if targetPkg == "" && len(activePackages) > 0 {
			targetPkg = activePackages[0]
		}

		displayName := getCloneDisplayName(targetPkg)

		currTime := time.Now().Format("15:04:05")
		safeLog("\n[%s] %s[ALERT]%s     %s%s%s reported: %s%s%s (%s)",
			currTime, Red, NC, White, displayName, NC, Amber, reason, NC, detail)
		writeLog("ERROR", fmt.Sprintf("%s: %s - %s", displayName, reason, detail))

		sendWebhook("In-Game Kick / Disconnect",
			fmt.Sprintf("**%s**\n**Player:** `%s`\n**Status:** `%s`\n**Detail:** ```%s```\nForce-stopping and relaunching...",
				displayName, player, reason, truncate(detail, 500)),
			15158332)

		recoveringMu.Lock()
		if recoveringClones[targetPkg] {
			recoveringMu.Unlock()
			return
		}
		recoveringClones[targetPkg] = true
		recoveringMu.Unlock()

		go recoverClone(targetPkg, displayName, false)
	})

	// 4. Log endpoint
	mux.HandleFunc("/log", func(w http.ResponseWriter, r *http.Request) {
		player := strings.TrimSpace(r.URL.Query().Get("player"))
		tag := strings.TrimSpace(r.URL.Query().Get("tag"))
		msg := strings.TrimSpace(r.URL.Query().Get("msg"))
		pkgParam := strings.TrimSpace(r.URL.Query().Get("pkg"))
		cloneParam := strings.TrimSpace(r.URL.Query().Get("clone"))

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))

		if msg == "" {
			return
		}

		targetPkg := pkgParam
		if targetPkg == "" && player != "" {
			playerPkgMu.RLock()
			targetPkg = playerPkgMap[player]
			playerPkgMu.RUnlock()
		}
		if targetPkg == "" {
			recentlyLaunchedMu.Lock()
			targetPkg = recentlyLaunchedPkg
			recentlyLaunchedMu.Unlock()
		}
		if targetPkg == "" && len(activePackages) > 0 {
			targetPkg = activePackages[0]
		}

		displayName := getCloneDisplayName(targetPkg)
		if cloneParam != "" {
			displayName = fmt.Sprintf("Clone %s", cloneParam)
		}

		currTime := time.Now().Format("15:04:05")
		tagColor := Cyan
		if strings.Contains(tag, "RECONNECT") {
			tagColor = Amber
		} else if strings.Contains(tag, "KICK") || strings.Contains(tag, "ERROR") {
			tagColor = Red
		} else if strings.Contains(tag, "SUCCESS") || strings.Contains(tag, "OK") {
			tagColor = Green
		}

		if player != "" {
			safeLog("[%s] %s[%s]%s %s%s%s (%s): %s", currTime, tagColor, tag, NC, White, displayName, NC, player, msg)
		} else {
			safeLog("[%s] %s[%s]%s %s%s%s: %s", currTime, tagColor, tag, NC, White, displayName, NC, msg)
		}
		writeLog(tag, fmt.Sprintf("%s: %s", displayName, msg))
	})

	server := &http.Server{
		Addr:    ":21420",
		Handler: mux,
	}

	_ = server.ListenAndServe()
}

func startCloudSignalPoller() {
	client := &http.Client{Timeout: 4 * time.Second}
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	type SignalResponse struct {
		HasSignal  bool   `json:"has_signal"`
		Pkg        string `json:"pkg"`
		Player     string `json:"player"`
		ErrorType  string `json:"error_type"`
		Detail     string `json:"detail"`
		ReportedAt int64  `json:"reported_at"`
	}

	for range ticker.C {
		if myHWID == "" {
			continue
		}

		pollURL := fmt.Sprintf("%s/check-signal?hwid=%s", AuthAPIURL, myHWID)
		resp, err := client.Get(pollURL)
		if err != nil || resp.StatusCode != 200 {
			if resp != nil {
				_ = resp.Body.Close()
			}
			continue
		}

		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		var sig SignalResponse
		if err := json.Unmarshal(body, &sig); err == nil && sig.HasSignal {
			targetPkg := sig.Pkg
			if targetPkg == "" && sig.Player != "" {
				playerPkgMu.RLock()
				targetPkg = playerPkgMap[sig.Player]
				playerPkgMu.RUnlock()
			}
			if targetPkg == "" {
				recentlyLaunchedMu.Lock()
				targetPkg = recentlyLaunchedPkg
				recentlyLaunchedMu.Unlock()
			}
			if targetPkg == "" && len(activePackages) > 0 {
				targetPkg = activePackages[0]
			}

			displayName := getCloneDisplayName(targetPkg)
			currTime := time.Now().Format("15:04:05")

			// Check if it's an online or reconnect status signal
			if sig.ErrorType == "ONLINE" || sig.ErrorType == "LUA_ONLINE" {
				safeLog("[%s] %s[CONNECT]%s   %s connected and monitoring.",
					currTime, Green, NC, displayName)
				writeLog("CONNECT", fmt.Sprintf("%s: Online", displayName))
				continue
			}

			if strings.Contains(sig.ErrorType, "RECONNECT") && !strings.Contains(sig.ErrorType, "FAILED") {
				safeLog("[%s] %s[RECONNECT]%s %s: %s",
					currTime, Amber, NC, displayName, sig.Detail)
				writeLog("RECONNECT", fmt.Sprintf("%s: %s", displayName, sig.Detail))
				continue
			}

			safeLog("\n[%s] %s[SIGNAL]%s    %s reported: %s%s%s (%s)",
				currTime, Red, NC, displayName, Amber, sig.ErrorType, NC, sig.Detail)
			writeLog("CLOUD_SIGNAL", fmt.Sprintf("%s: %s - %s", displayName, sig.ErrorType, sig.Detail))

			sendWebhook("In-Game Kick (Cloud Signal)",
				fmt.Sprintf("**%s**\n**Player:** `%s`\n**Status:** `%s`\n**Detail:** ```%s```\nForce-stopping and relaunching...",
					displayName, sig.Player, sig.ErrorType, truncate(sig.Detail, 500)),
				15158332)

			recoveringMu.Lock()
			if recoveringClones[targetPkg] {
				recoveringMu.Unlock()
				continue
			}
			recoveringClones[targetPkg] = true
			recoveringMu.Unlock()

			go recoverClone(targetPkg, displayName, false)
		}
	}
}

func startEventLogWatcher() {
	logPath := "/sdcard/nefarious_events.log"
	_ = os.Remove(logPath)

	var lastOffset int64 = 0
	for {
		time.Sleep(500 * time.Millisecond)
		fi, err := os.Stat(logPath)
		if err != nil || fi.Size() <= lastOffset {
			continue
		}

		f, err := os.Open(logPath)
		if err != nil {
			continue
		}
		_, _ = f.Seek(lastOffset, 0)
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}

			currTime := time.Now().Format("15:04:05")
			tagColor := Cyan
			if strings.Contains(line, "RECONNECT") {
				tagColor = Amber
			} else if strings.Contains(line, "KICK") || strings.Contains(line, "ERROR") {
				tagColor = Red
			} else if strings.Contains(line, "SUCCESS") {
				tagColor = Green
			}

			safeLog("[%s] %s[SENTINEL]%s  %s", currTime, tagColor, NC, cleanSentinelLogLine(line))
			writeLog("SENTINEL_LOG", line)

			if strings.Contains(line, "KICK_DETECTED") || strings.Contains(line, "RECONNECT_FAILED") {
				for i, p := range activePackages {
					cloneTag := fmt.Sprintf("Clone %d", i+1)
					if strings.Contains(line, cloneTag) || strings.Contains(line, p) {
						recoveringMu.Lock()
						if !recoveringClones[p] {
							recoveringClones[p] = true
							recoveringMu.Unlock()
							go recoverClone(p, cloneTag, false)
						} else {
							recoveringMu.Unlock()
						}
					}
				}
			}
		}
		lastOffset, _ = f.Seek(0, io.SeekCurrent)
		_ = f.Close()
	}
}

func startKickSignalWatcher() {
	signalFile := "/sdcard/nefarious_kick_signal.txt"
	for {
		time.Sleep(500 * time.Millisecond)
		data, err := os.ReadFile(signalFile)
		if err != nil || len(data) == 0 {
			continue
		}
		_ = os.Remove(signalFile)

		raw := strings.TrimSpace(string(data))
		parts := strings.Split(raw, "|")
		targetPkg := parts[0]
		displayName := getCloneDisplayName(targetPkg)
		if len(parts) > 1 && strings.TrimSpace(parts[1]) != "" {
			candidate := strings.TrimSpace(parts[1])
			if strings.Contains(candidate, "Clone") {
				displayName = candidate
			}
		}
		if targetPkg == "" && len(activePackages) > 0 {
			targetPkg = activePackages[0]
			displayName = "Clone 1"
		}

		currTime := time.Now().Format("15:04:05")
		safeLog("\n[%s] %s[AUTO-REJOIN]%s %s disconnected. Rejoining %s%s%s...",
			currTime, Amber, NC, displayName, White, gameName, NC)

		recoveringMu.Lock()
		if !recoveringClones[targetPkg] {
			recoveringClones[targetPkg] = true
			recoveringMu.Unlock()
			go recoverClone(targetPkg, displayName, false)
		} else {
			recoveringMu.Unlock()
		}
	}
}

func main() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		fmt.Println("\nSession terminated by user.")
		os.Exit(0)
	}()

	initInputReader()

	myHWID = getDeviceHWID()

	drawBanner()
	waitForInternetAtStartup()
	checkUpdates()
	verifyLicense()

	drawBanner()
	configureConcurrency()
	configureTargetExperience()
	configureSentinel()
	configureWebhook()

	drawBanner()
	drawSummaryCard()
	fmt.Println()

	launchInitialInstances()

	if enableRejoin {
		go startLocalBridgeServer()
		go startCloudSignalPoller()
		go startEventLogWatcher()
		go startKickSignalWatcher()
		go startNetworkMonitor()
		startSentinelMonitor()
	}
}

