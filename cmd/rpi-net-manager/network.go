package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	netmanagerclient "github.com/TheCacophonyProject/rpi-net-manager/netmanagerclient"
	"github.com/godbus/dbus/v5"
)

type networkStateMachine struct {
	mux                       sync.Mutex
	state                     netmanagerclient.NetworkState
	connName                  string
	wifiScanConnectTimer      *time.Timer
	wifiScanTimer             *time.Timer
	hotspotTimer              *time.Timer
	keepHotspotOnUntil        time.Time
	NetworkUpdateChannel      chan struct{}
	hotspotFallback           bool
	hotspotInterface          string
	hotspotPreferredInterface string
	lastWifiInterface         string
	hostapdActive             bool
	hostapdCmd                *exec.Cmd
	scanFailureCount          int
}

var errNoActiveAP = errors.New("no active AP found")

const hotspotInterfaceConfigPath = "/var/lib/cacophony/rpi-net-manager/hotspot-interface"

func loadHotspotInterfacePreference() (string, error) {
	data, err := os.ReadFile(hotspotInterfaceConfigPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("failed to read hotspot interface preference: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

func saveHotspotInterfacePreference(iface string) error {
	dir := filepath.Dir(hotspotInterfaceConfigPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to prepare hotspot interface config dir: %w", err)
	}
	iface = strings.TrimSpace(iface)
	return os.WriteFile(hotspotInterfaceConfigPath, []byte(iface), 0o644)
}

func (nsm *networkStateMachine) hotspotIfname() string {
	if override := strings.TrimSpace(nsm.hotspotPreferredInterface); override != "" {
		if !interfaceExists(override) {
			if nsm.hotspotInterface == override {
				nsm.hotspotInterface = ""
			}
			log.Printf("Configured hotspot interface %s is unavailable; falling back to automatic selection", override)
		} else {
			if override != nsm.hotspotInterface {
				log.Printf("Hotspot interface override set to %s", override)
			}
			nsm.hotspotInterface = override
			return nsm.hotspotInterface
		}
	}

	if primary := primaryWifiInterface(); primary != "" {
		if primary != nsm.hotspotInterface {
			log.Printf("Hotspot interface updated from %s to %s", nsm.hotspotInterface, primary)
		}
		nsm.hotspotInterface = primary
		return nsm.hotspotInterface
	}

	nsm.hotspotInterface = ""
	return ""
}

func (nsm *networkStateMachine) availableHotspotInterfaces() ([]string, error) {
	ifaces, err := wifiInterfaces()
	if err != nil {
		return nil, err
	}
	ordered := []string{}
	seen := map[string]struct{}{}

	if primary := primaryWifiInterface(); primary != "" && interfaceExists(primary) {
		seen[primary] = struct{}{}
		ordered = append(ordered, primary)
	}

	for _, iface := range ifaces {
		if _, ok := seen[iface]; ok {
			continue
		}
		seen[iface] = struct{}{}
		ordered = append(ordered, iface)
	}
	return ordered, nil
}

func (nsm *networkStateMachine) setHotspotInterfacePreference(iface string) error {
	iface = strings.TrimSpace(iface)
	if iface != "" {
		ifaces, err := wifiInterfaces()
		if err != nil {
			return err
		}
		found := slices.Contains(ifaces, iface)
		if !found {
			return fmt.Errorf("%s is not an available Wi-Fi interface", iface)
		}
	}

	if err := saveHotspotInterfacePreference(iface); err != nil {
		return err
	}

	if iface == "" {
		log.Println("Cleared hotspot interface preference (automatic selection enabled)")
	} else if iface != nsm.hotspotPreferredInterface {
		log.Printf("Hotspot interface preference updated to %s", iface)
	}

	nsm.hotspotPreferredInterface = iface
	if nsm.hotspotInterface != iface {
		nsm.hotspotInterface = ""
	}
	nsm.hotspotFallback = true

	if nsm.state == netmanagerclient.NS_HOTSPOT_RUNNING || nsm.state == netmanagerclient.NS_HOTSPOT_STARTING {
		if err := nsm.setupWifi(); err != nil {
			return fmt.Errorf("failed to restart hotspot while applying interface preference: %w", err)
		}
		if err := nsm.setupHotspot(); err != nil {
			target := iface
			if target == "" {
				target = "automatic interface"
			}
			return fmt.Errorf("failed to bring hotspot up on %s: %w", target, err)
		}
	}
	return nil
}

func (nsm *networkStateMachine) currentWifiDevice() string {
	if nsm.connName != "" && nsm.connName != bushnetHotspot {
		iface, err := getConnectionDevice(nsm.connName)
		if err != nil {
			log.Printf("failed to read active device for %s: %v", nsm.connName, err)
			return nsm.lastWifiInterface
		}
		if iface != "" {
			nsm.lastWifiInterface = iface
		}
		return iface
	}
	return nsm.lastWifiInterface
}

func (nsm *networkStateMachine) ensureWifiDevicesManaged() {
	out, err := exec.Command("nmcli", "-t", "-f", "DEVICE,TYPE,STATE", "device", "status").CombinedOutput()
	if err != nil {
		log.Printf("ensureWifiDevicesManaged: nmcli error: %v, output: %s", err, out)
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		p := strings.Split(line, ":")
		if len(p) < 3 || p[1] != "wifi" {
			continue
		}
		if strings.Contains(strings.ToLower(p[2]), "unmanaged") {
			_ = runNMCli("device", "set", p[0], "managed", "yes")
		}
	}
}

func clearConnectionProperty(identifier, property string) (string, bool, error) {
	out, err := exec.Command("nmcli", "--get-values", property, "connection", "show", identifier).CombinedOutput()
	if err != nil {
		return "", false, fmt.Errorf("nmcli get %s for %s: %w, output: %s", property, identifier, err, out)
	}

	value := ""
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		if i := strings.Index(line, ":"); i >= 0 {
			line = strings.TrimSpace(line[i+1:])
		}
		line = strings.TrimSpace(line)
		if line == "" || line == "--" {
			continue
		}
		value = line
		break
	}
	if value == "" {
		return "", false, nil
	}

	if err := runNMCli("connection", "modify", identifier, property, ""); err != nil {
		return "", false, err
	}
	return value, true, nil
}

// Unpin all Wi-Fi profiles so NM can move them between wlan1/wlan0.
// Uses UUID to avoid name/colon/space edge cases.
func (nsm *networkStateMachine) clearPinsForAllWifiProfiles() {
	out, err := exec.Command("nmcli", "-t", "-f", "UUID,TYPE,NAME", "connection", "show").CombinedOutput()
	if err != nil {
		log.Printf("clearPinsForAllWifiProfiles: nmcli list error: %v, output: %s", err, out)
		return
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) < 3 {
			continue
		}
		uuid, typ, name := parts[0], parts[1], parts[2]
		if typ != "802-11-wireless" {
			continue
		}
		if prev, cleared, err := clearConnectionProperty(uuid, "connection.interface-name"); err != nil {
			log.Printf("clearPinsForAllWifiProfiles: failed to clear interface-name for %s (%s): %v", name, uuid, err)
		} else if cleared {
			log.Printf("Cleared interface pin for %s (was '%s')", name, prev)
		}
		if prev, cleared, err := clearConnectionProperty(uuid, "802-11-wireless.mac-address"); err != nil {
			log.Printf("clearPinsForAllWifiProfiles: failed to clear mac-address for %s (%s): %v", name, uuid, err)
		} else if cleared {
			log.Printf("Cleared mac-address pin for %s (was '%s')", name, prev)
		}
		if prev, cleared, err := clearConnectionProperty(uuid, "802-11-wireless.cloned-mac-address"); err != nil {
			log.Printf("clearPinsForAllWifiProfiles: failed to clear cloned-mac-address for %s (%s): %v", name, uuid, err)
		} else if cleared {
			log.Printf("Cleared cloned-mac-address pin for %s (was '%s')", name, prev)
		}
	}
}

func (nsm *networkStateMachine) handleStateTransition(newState netmanagerclient.NetworkState, newConName string) error {
	oldConName := nsm.connName
	nsm.connName = newConName
	oldState := nsm.state
	if newState == oldState {
		return nil
	}
	nsm.setState(newState)
	log.Printf("State transition: %s -> %s, Active Connection: '%s'", oldState, newState, newConName)
	nsm.logBssid()

	if newState == netmanagerclient.NS_WIFI_SCANNING && oldConName != "" && oldConName != bushnetHotspot {
		nsm.ensurePreferredClientInterface(oldConName)
	}
	if newState == netmanagerclient.NS_WIFI_CONNECTING && newConName != "" && newConName != bushnetHotspot {
		nsm.ensurePreferredClientInterface(newConName)
	}

	// If going from CONNECTING to SCANNING then the connection probably failed.
	if oldState == netmanagerclient.NS_WIFI_CONNECTING && newState == netmanagerclient.NS_WIFI_SCANNING {
		// Check if connection failed. Note that this can be for multiple different reasons, wrong password, bad connection...
		threeSecondsAgo := time.Now().Add(-3 * time.Second).Format("2006-01-02 15:04:05")
		out, err := exec.Command("journalctl", "-u", "NetworkManager", "--no-pager", "--since", threeSecondsAgo).CombinedOutput()
		if err != nil {
			return err
		}
		if strings.Contains(string(out), fmt.Sprintf("Activation: failed for connection '%s'", oldConName)) {
			log.Printf("Failed to connect to '%s'", oldConName)
			// Set auth retries to 1, this will make it fail sooner in the future if it fails again.
			// Don't want to disable autoconnect because it might be some other issue causing it to fail to connect.
			// If it is successfully connect to in the future it will be set back to 2.
			// Reading the value of auth-retries is used to determine if the connection has failed.
			if err := runNMCli("connection", "modify", oldConName, "connection.auth-retries", "1"); err != nil {
				log.Printf("failed to set auth-retries to 1, '%s'", err)
			}
		}
	}

	switch newState {
	case netmanagerclient.NS_WIFI_SCANNING:
		log.Println("Restarting wifi scan timer")
		resetTimer(&nsm.wifiScanTimer, 10*time.Second)
		// Reset timer for wifi to scan and connect to a network.
		if oldState != netmanagerclient.NS_WIFI_CONNECTING {
			resetTimer(&nsm.wifiScanConnectTimer, 10*time.Minute)
		}

		target := preferredClientInterface()
		if target != "" && interfaceExists(target) {
			nsm.ensureWifiDevicesManaged()

			nsm.clearPinsForAllWifiProfiles()

			// Kick NM to (re)connect that device using best autoconnect profile
			if err := runNMCli("device", "connect", target); err != nil {
				log.Printf("device connect %s failed: %v", target, err)
			}
		}

	case netmanagerclient.NS_HOTSPOT_RUNNING:
		// Reset timer for hotspot when it has started up.
		resetTimer(&nsm.hotspotTimer, 5*time.Minute)

	case netmanagerclient.NS_WIFI_CONNECTED:
		// Set auth retries to 2 in case it was set to 1 previously.
		if err := runNMCli("connection", "modify", newConName, "connection.auth-retries", "2"); err != nil {
			log.Printf("failed to set auth-retries to 2, '%s'", err)
		}
		nsm.ensurePreferredClientInterface(newConName)
		nsm.hotspotFallback = true
	}
	return nil
}

// Utility function to safely reset a timer
func resetTimer(t **time.Timer, d time.Duration) {
	if *t == nil {
		*t = time.NewTimer(d)
		return
	}
	if !(*t).Stop() {
		select {
		case <-(*t).C: // drain if needed
		default:
		}
	}
	(*t).Reset(d)
}

func (nsm *networkStateMachine) runStateMachine() error {
	log.Println("Starting Network Manager state machine")
	if err := runNMCli("radio", "wifi", "on"); err != nil {
		return err
	}
	// Make sure we never get stuck on a missing iface.
	nsm.ensureWifiDevicesManaged()
	nsm.clearPinsForAllWifiProfiles()

	// Timeout flags
	wifiScanConnectTimeout := false
	wifiScanTimeout := false
	hotspotTimeout := false

	nsm.mux.Lock()
	defer nsm.mux.Unlock()

	for {
		// Look at the network setup to determine what network state the device is in.
		newState, conName, err := nsm.detectState()
		if err != nil {
			return err
		}
		if nsm.hostapdActive && newState == netmanagerclient.NS_WIFI_SCANNING {
			newState = netmanagerclient.NS_HOTSPOT_RUNNING
			if conName == "" {
				conName = bushnetHotspot
			}
		}
		// Handle state transitions, this will reset appropriate timers if needed.
		err = nsm.handleStateTransition(newState, conName)
		if err != nil {
			return err
		}

		if nsm.state != netmanagerclient.NS_WIFI_SCANNING {
			nsm.scanFailureCount = 0
		}

		// Update the state
		switch nsm.state {
		case netmanagerclient.NS_WIFI_SCANNING:
			if wifiScanConnectTimeout {
				wifiScanConnectTimeout = false
				log.Println("Wifi scan connect timeout, powering off wifi")
				if err := runNMCli("radio", "wifi", "off"); err != nil {
					return err
				}
			}
			if wifiScanTimeout {
				wifiScanTimeout = false
				nsm.scanFailureCount++
				if nsm.scanFailureCount < hotspotScanFailureThreshold {
					log.Printf("Wifi scan timeout %d/%d - continuing to scan", nsm.scanFailureCount, hotspotScanFailureThreshold)
					break
				}
				nsm.scanFailureCount = 0
				if nsm.hotspotFallback {
					// Checking if the hotspot should turn on.
					minutes, err := getMinutesSinceHumanInteraction()
					log.Info("Minutes since human interaction:", minutes)
					if err != nil {
						return fmt.Errorf("failed to get minutes since human interaction: %v", err)
					}
					if minutes > 60 {
						log.Info("Not falling back to hosting hotspot as there has not been a user interaction in 60 minutes")
						break
					}
					nsm.hotspotFallback = false
					log.Info("Enable hotspot")
					if err := nsm.setupHotspot(); err != nil {
						return err
					}
				}
			}
		case netmanagerclient.NS_WIFI_OFF:
			// Turn wifi back on if button is pressed, this will get handled elsewhere.
		case netmanagerclient.NS_WIFI_CONNECTING:
			// Nothing to do
		case netmanagerclient.NS_WIFI_CONNECTED:
			// Nothing to do
		case netmanagerclient.NS_HOTSPOT_STARTING:
			// Nothing to do
		case netmanagerclient.NS_HOTSPOT_RUNNING:
			if hotspotTimeout {
				hotspotTimeout = false
				log.Println("Hotspot timeout, powering off hotspot")
				nsm.setupWifi() // Enabling wifi will disable the hotspot, then it will scan the network once again then.
			}
		default:
			log.Error("Unhandled network state:", nsm.state)
		}

		nsm.mux.Unlock()
		select {
		case <-nsm.wifiScanConnectTimer.C:
			if nsm.state == netmanagerclient.NS_WIFI_SCANNING || nsm.state == netmanagerclient.NS_WIFI_CONNECTING {
				// log.Println("Wifi scan connect timeout")
				wifiScanConnectTimeout = true
			}
		case <-nsm.wifiScanTimer.C:
			if nsm.state == netmanagerclient.NS_WIFI_SCANNING {
				// log.Println("Wifi scan timeout")
				wifiScanTimeout = true
			}
		case <-nsm.hotspotTimer.C:
			if nsm.state == netmanagerclient.NS_HOTSPOT_RUNNING {
				// log.Println("Hotspot timeout")
				hotspotTimeout = true
			}
		case <-nsm.NetworkUpdateChannel:
			// log.Println("Network update")
		}
		nsm.mux.Lock()
	}
}

func getActiveBSSIDFromOutput(output string) (string, error) {
	lines := strings.Split(output, "\n")
	inUseAPID := ""
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, ".IN-USE:*") {
			inUseAPID = strings.Split(line, ".")[0]
			break
		}
	}

	if inUseAPID == "" {
		return "", errNoActiveAP
	}

	for _, line := range lines {
		if strings.HasPrefix(line, inUseAPID+".BSSID:") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				return parts[1], nil
			}
		}
	}

	return "", fmt.Errorf("no BSSID found")
}

func (nsm *networkStateMachine) logBssid() {
	seen := map[string]struct{}{}
	candidates := []string{}

	if active := nsm.currentWifiDevice(); active != "" {
		seen[active] = struct{}{}
		candidates = append(candidates, active)
	}

	if cached := nsm.hotspotInterface; cached != "" {
		if _, ok := seen[cached]; !ok {
			seen[cached] = struct{}{}
			candidates = append(candidates, cached)
		}
	}

	for _, iface := range []string{"wlan0", "wlan1"} {
		if _, ok := seen[iface]; ok {
			continue
		}
		seen[iface] = struct{}{}
		candidates = append(candidates, iface)
	}

	for _, iface := range candidates {
		if !interfaceExists(iface) {
			continue
		}
		bssidOutRaw, err := exec.Command("nmcli", "--terse", "--fields", "AP.BSSID,AP.IN-USE", "device", "show", iface).CombinedOutput()
		if err != nil {
			log.Printf("failed to run nmcli for %s: %v, output: %s", iface, err, bssidOutRaw)
			continue
		}
		bssid, err := getActiveBSSIDFromOutput(string(bssidOutRaw))
		if err != nil {
			if errors.Is(err, errNoActiveAP) {
				continue
			}
			log.Printf("failed to get active bssid for %s: %v, output: %s", iface, err, bssidOutRaw)
			continue
		}
		log.Info("Active BSSID:", bssid)
		return
	}
}

func (nsm *networkStateMachine) ensurePreferredClientInterface(connection string) {
	if connection == "" || connection == bushnetHotspot {
		return
	}
	target := preferredClientInterface()
	if target == "" || !interfaceExists(target) {
		return
	}

	// Make sure this profile is not pinned to a specific iface.
	if prev, cleared, err := clearConnectionProperty(connection, "connection.interface-name"); err != nil {
		log.Printf("failed to clear interface-name binding for %s: %v", connection, err)
	} else if cleared {
		log.Printf("Cleared interface pin for %s (was '%s')", connection, prev)
	}
	if prev, cleared, err := clearConnectionProperty(connection, "802-11-wireless.mac-address"); err != nil {
		log.Printf("failed to clear mac-address binding for %s: %v", connection, err)
	} else if cleared {
		log.Printf("Cleared mac-address pin for %s (was '%s')", connection, prev)
	}
	if prev, cleared, err := clearConnectionProperty(connection, "802-11-wireless.cloned-mac-address"); err != nil {
		log.Printf("failed to clear cloned-mac-address binding for %s: %v", connection, err)
	} else if cleared {
		log.Printf("Cleared cloned-mac-address pin for %s (was '%s')", connection, prev)
	}

	nsm.lastWifiInterface = target

	// If it's already active on the wrong iface, move it using an ephemeral ifname.
	active, err := getConnectionDevice(connection)
	if err != nil {
		log.Printf("failed to read active device for %s: %v", connection, err)
		return
	}
	if active == "" || active == target {
		// Inactive: SCANNING path will `nmcli device connect <target>`
		// Already correct: nothing to do.
		return
	}

	_ = runNMCli("connection", "down", connection)
	if err := runNMCli("connection", "up", connection, "ifname", target); err != nil {
		log.Printf("failed to bring connection %s up on %s: %v", connection, target, err)
	}
}

func getConnectionDevice(connection string) (string, error) {
	out, err := exec.Command("nmcli", "-t", "-f", "GENERAL.DEVICES", "connection", "show", connection).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("nmcli error: %w, output: %s", err, out)
	}
	line := strings.TrimSpace(string(out))
	if line == "" || strings.HasSuffix(line, ":--") {
		return "", nil
	}
	parts := strings.SplitN(line, ":", 2)
	if len(parts) == 2 {
		return strings.TrimSpace(parts[1]), nil
	}
	if strings.Contains(line, ",") {
		return strings.TrimSpace(strings.Split(line, ",")[0]), nil
	}
	return strings.TrimSpace(line), nil
}

func (nsm *networkStateMachine) keepHotspotOnFor(keepOnFor time.Duration) {
	newKeepOnUntil := time.Now().Add(keepOnFor)
	if newKeepOnUntil.After(nsm.keepHotspotOnUntil) {
		log.Println("Keep hotspot on for", keepOnFor)
		nsm.keepHotspotOnUntil = newKeepOnUntil
		resetTimer(&nsm.hotspotTimer, keepOnFor)
	} else {
		log.Printf("Keep hotspot on for %s, but already on for %s", keepOnFor, time.Until(nsm.keepHotspotOnUntil))
	}
}

func (nsm *networkStateMachine) setState(ns netmanagerclient.NetworkState) {
	if nsm.state != ns {
		log.Printf("State changed from %s to %s", nsm.state, ns)
		nsm.state = ns
		err := sendNewNetworkState(ns)
		if err != nil {
			log.Println(err)
		}
	}
}

func runNMCli(args ...string) error {
	out, err := exec.Command("nmcli", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to run nmcli: %v, output: %s", err, out)
	}
	return nil
}

const bushnetHotspot = "BushnetHotspot"

func (nsm *networkStateMachine) setupHotspot() error {
	nsm.setState(netmanagerclient.NS_HOTSPOT_STARTING)
	log.Println("Turn wifi radio on.")
	if err := runNMCli("radio", "wifi", "on"); err != nil {
		return err
	}

	iface := nsm.hotspotIfname()
	if iface == "" {
		return fmt.Errorf("no wireless interface available for hotspot")
	}
	log.Printf("Setting up network for hosting a hotspot on %s.", iface)
	band, channel, supportsVHT := selectHotspotRadioConfig(iface)
	if band == "" {
		band = "bg"
	}
	usingFiveGHz := band == "a" || channel > 0
	useHostapd := iface == "wlan1"

	if useHostapd {
		if hostapdErr := nsm.startHostapdHotspot(iface, band, channel, supportsVHT); hostapdErr == nil {
			return nil
		} else {
			log.Printf("hostapd hotspot start failed on %s: %v", iface, hostapdErr)
		}
		if usingFiveGHz {
			log.Printf("Falling back to 2.4GHz hostapd on %s", iface)
			downgradeErr := nsm.startHostapdHotspot(iface, "bg", 1, false)
			if downgradeErr == nil {
				return nil
			}
			log.Printf("hostapd 2.4GHz fallback failed: %v", downgradeErr)
		}
		if stopErr := nsm.stopHostapdHotspot(); stopErr != nil {
			log.Printf("failed to ensure hostapd stopped before NM fallback: %v", stopErr)
		}
	}
	hotspotConfig := map[string]string{
		"connection.type":              "802-11-wireless",
		"ifname":                       iface,
		"autoconnect":                  "no",
		"ssid":                         "bushnet",
		"802-11-wireless.mode":         "ap",
		"ipv4.method":                  "manual", // Using 'manual' instead of 'shared' so can configure dnsmasq to not share the internet connection of the modem to connected devices.
		"wifi-sec.key-mgmt":            "wpa-psk",
		"wifi-sec.psk":                 "feathers",
		"ipv4.addresses":               router_ip + "/24",
		"802-11-wireless-security.pmf": "disable", // Android has issues with PMF
	}

	if band != "" {
		hotspotConfig["802-11-wireless.band"] = band
	}

	if channel > 0 {
		hotspotConfig["802-11-wireless.channel"] = strconv.Itoa(channel)
	}

	if err := netmanagerclient.ModifyNetworkConfig(bushnetHotspot, hotspotConfig); err != nil {
		errText := err.Error()
		if strings.Contains(errText, "is not a valid channel") || strings.Contains(errText, "invalid channel") {
			if channel > 0 {
				log.Printf("Hotspot channel %d not supported on %s; downgrading to 2.4GHz", channel, iface)
			} else {
				log.Printf("Hotspot channel not supported on %s; downgrading to 2.4GHz", iface)
			}
			if channel > 0 {
				if clearErr := runNMCli("connection", "modify", bushnetHotspot, "802-11-wireless.channel", ""); clearErr != nil {
					log.Printf("failed to clear hotspot channel property: %v", clearErr)
				}
			}
			hotspotConfig["802-11-wireless.band"] = "bg"
			channel = 0
			if retryErr := netmanagerclient.ModifyNetworkConfig(bushnetHotspot, hotspotConfig); retryErr != nil {
				return retryErr
			}
		} else {
			return err
		}
	}
	log.Println("Starting hotspot...")
	if err := runNMCli("connection", "up", bushnetHotspot); err != nil {
		if usingFiveGHz {
			log.Printf("Hotspot failed to start on %s using 5GHz (%v); retrying on 2.4GHz", iface, err)
			fallbackConfig := map[string]string{
				"802-11-wireless.band": "bg",
			}
			if applyErr := netmanagerclient.ModifyNetworkConfig(bushnetHotspot, fallbackConfig); applyErr != nil {
				log.Printf("failed to apply 2.4GHz fallback config: %v", applyErr)
				return err
			}
			if channel > 0 {
				if clearErr := runNMCli("connection", "modify", bushnetHotspot, "802-11-wireless.channel", ""); clearErr != nil {
					log.Printf("failed to clear hotspot channel property: %v", clearErr)
				}
			}
			if retryErr := runNMCli("connection", "up", bushnetHotspot); retryErr != nil {
				return retryErr
			}
		} else {
			return err
		}
	}

	if err := createDNSConfig(iface, "192.168.4.2,192.168.4.20"); err != nil {
		return err
	}

	log.Printf("Starting DNS...")
	if err := exec.Command("systemctl", "restart", "dnsmasq").Run(); err != nil {
		return err
	}
	return nil
}

func (nsm *networkStateMachine) detectState() (netmanagerclient.NetworkState, string, error) {
	out, err := exec.Command("nmcli", "radio", "wifi").CombinedOutput()
	if err != nil {
		return netmanagerclient.NS_ERROR, "", fmt.Errorf("error getting wifi radio state %s, err: %s", out, err)
	}
	radioState := strings.TrimSpace(string(out))
	if radioState == "disabled" {
		if nsm != nil && nsm.hostapdActive {
			return netmanagerclient.NS_HOTSPOT_RUNNING, bushnetHotspot, nil
		}
		return netmanagerclient.NS_WIFI_OFF, "", nil
	} else if radioState != "enabled" {
		return netmanagerclient.NS_ERROR, "", fmt.Errorf("unknown radio state '%s'", radioState)
	}

	// Get name of active network, if there is one.
	wifiConnectionName := ""
	out, err = exec.Command("nmcli", "--terse", "--fields", "TYPE,NAME", "connection", "show", "--active").CombinedOutput()
	if err != nil {
		return netmanagerclient.NS_ERROR, "", fmt.Errorf("error running list of active connections %s, err: %s", out, err)
	}
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		parts := strings.Split(line, ":")
		if len(parts) < 2 {
			continue
		}
		if parts[0] == "802-11-wireless" {
			wifiConnectionName = strings.Join(parts[1:], ":")
		}
	}

	// No active network found, wifi is just scanning
	if wifiConnectionName == "" {
		if nsm != nil && nsm.hostapdActive {
			return netmanagerclient.NS_HOTSPOT_RUNNING, bushnetHotspot, nil
		}
		return netmanagerclient.NS_WIFI_SCANNING, "", nil
	}
	// Active network is the bushnet hotspot
	if wifiConnectionName == bushnetHotspot {
		return netmanagerclient.NS_HOTSPOT_RUNNING, wifiConnectionName, nil
	}

	// Connected to a network. Check if it has a IP address.
	// If it doesn't have an IP address it is still trying to connect, could have the wrong password.
	out, err = exec.Command("nmcli", "--terse", "--fields", "IP4.ADDRESS", "connection", "show", wifiConnectionName).CombinedOutput()
	if err != nil {
		return netmanagerclient.NS_ERROR, "", fmt.Errorf("error checking ip address of connection: %s, err: %s", out, err)
	}

	ipAddress := strings.TrimPrefix(string(out), "IP4.ADDRESS[1]:")
	ipAddress = strings.TrimSpace(ipAddress)

	if ipAddress == "" {
		return netmanagerclient.NS_WIFI_CONNECTING, wifiConnectionName, nil
	} else {
		return netmanagerclient.NS_WIFI_CONNECTED, wifiConnectionName, nil
	}
}

const (
	router_ip                   = "192.168.4.1"
	hostapdConfigPath           = "/etc/hostapd/hostapd-rpi-net-manager.conf"
	hotspotScanFailureThreshold = 6
)

func createDNSConfig(iface, ipRange string) error {
	if iface == "" {
		return fmt.Errorf("dnsmasq interface not specified")
	}
	fileName := "/etc/dnsmasq.conf"
	configLines := []string{
		"interface=" + iface,
		"dhcp-range=" + ipRange + ",12h",
		"domain=wlan",
		"server=1.1.1.1",
		"server=8.8.8.8",
	}
	return createConfigFile(fileName, configLines)
}

func createConfigFile(name string, config []string) error {
	file, err := os.Create(name)
	if err != nil {
		return err
	}
	defer file.Close()

	w := bufio.NewWriter(file)
	for _, line := range config {
		_, err = fmt.Fprintln(w, line)
		if err != nil {
			return err
		}
	}
	err = w.Flush()
	if err != nil {
		return err
	}
	return nil
}

func streamPipe(prefix string, r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		log.Printf("%s: %s", prefix, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		log.Printf("%s: read error: %v", prefix, err)
	}
}

func (nsm *networkStateMachine) startHostapdHotspot(iface, band string, channel int, supportsVHT bool) error {
	if iface == "" {
		return fmt.Errorf("hostapd requires a wireless interface")
	}
	if nsm.hostapdActive {
		if err := nsm.stopHostapdHotspot(); err != nil {
			return fmt.Errorf("failed to stop existing hostapd instance: %w", err)
		}
	}

	if err := runNMCli("connection", "down", bushnetHotspot); err != nil {
		log.Printf("hostapd: unable to bring down NM hotspot connection: %v", err)
	}
	if err := runNMCli("device", "disconnect", iface); err != nil {
		log.Printf("hostapd: failed to disconnect %s from NM: %v", iface, err)
	}
	if err := runNMCli("device", "set", iface, "managed", "no"); err != nil {
		log.Printf("hostapd: failed to set %s unmanaged: %v", iface, err)
	}

	if err := exec.Command("ip", "link", "set", iface, "down").Run(); err != nil {
		return fmt.Errorf("failed to bring %s down: %w", iface, err)
	}
	if err := exec.Command("ip", "addr", "flush", "dev", iface).Run(); err != nil {
		log.Printf("hostapd: failed to flush addresses on %s: %v", iface, err)
	}
	if err := exec.Command("ip", "addr", "add", fmt.Sprintf("%s/24", router_ip), "dev", iface).Run(); err != nil {
		return fmt.Errorf("failed to assign IP to %s: %w", iface, err)
	}
	if err := exec.Command("ip", "link", "set", iface, "up").Run(); err != nil {
		return fmt.Errorf("failed to bring %s up: %w", iface, err)
	}

	country := detectRegulatoryCountry()
	if len(country) != 2 || strings.EqualFold(country, "00") {
		country = "NZ"
	}
	configLines, err := hostapdConfigLines(iface, band, channel, supportsVHT, country)
	if err != nil {
		return err
	}
	if err := createConfigFile(hostapdConfigPath, configLines); err != nil {
		return fmt.Errorf("failed to write hostapd config: %w", err)
	}
	cmd := exec.Command("/usr/sbin/hostapd", hostapdConfigPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to capture hostapd stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to capture hostapd stderr: %w", err)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start hostapd: %w", err)
	}
	go streamPipe("hostapd stdout", stdout)
	go streamPipe("hostapd stderr", stderr)
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	select {
	case err := <-done:
		if err != nil {
			log.Printf("hostapd exited immediately: %v", err)
		}
		nsm.hostapdCmd = nil
		nsm.hostapdActive = false
		return fmt.Errorf("hostapd failed to initialize: %w", err)
	default:
		nsm.hostapdCmd = cmd
		go nsm.monitorHostapd(done, cmd)
	}

	if err := createDNSConfig(iface, "192.168.4.2,192.168.4.20"); err != nil {
		nsm.stopHostapdHotspot()
		return err
	}
	restartDnsmasq := exec.Command("systemctl", "restart", "dnsmasq")
	if out, err := restartDnsmasq.CombinedOutput(); err != nil {
		nsm.stopHostapdHotspot()
		return fmt.Errorf("failed to restart dnsmasq: %w, output: %s", err, out)
	}

	nsm.hostapdActive = true
	return nil
}

func (nsm *networkStateMachine) monitorHostapd(done <-chan error, cmd *exec.Cmd) {
	err := <-done
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		log.Printf("hostapd process exited: %v", err)
	}
	nsm.mux.Lock()
	defer nsm.mux.Unlock()
	if nsm.hostapdCmd == cmd {
		nsm.hostapdCmd = nil
		nsm.hostapdActive = false
		nsm.hotspotFallback = true
	}
}

func (nsm *networkStateMachine) stopHostapdHotspot() error {
	iface := nsm.hotspotInterface
	// if iface is unknown, best-effort: try to restore management on common Wi-Fi ifaces
	if nsm.hostapdCmd != nil && nsm.hostapdCmd.Process != nil {
		if err := nsm.hostapdCmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			log.Printf("hostapd: failed to signal process: %v", err)
		}
		if err := nsm.hostapdCmd.Wait(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			log.Printf("hostapd: process wait error: %v", err)
		}
		nsm.hostapdCmd = nil
	}
	if iface != "" && interfaceExists(iface) {
		if err := exec.Command("ip", "addr", "flush", "dev", iface).Run(); err != nil {
			log.Printf("hostapd: failed to flush addresses on %s: %v", iface, err)
		}
		if err := exec.Command("ip", "link", "set", iface, "down").Run(); err != nil {
			log.Printf("hostapd: failed to bring %s down: %v", iface, err)
		}
	}
	if iface != "" {
		if err := runNMCli("device", "set", iface, "managed", "yes"); err != nil {
			log.Printf("hostapd: failed to return %s to NetworkManager: %v", iface, err)
		}
	}
	if iface != "" && interfaceExists(iface) {
		if err := exec.Command("ip", "link", "set", iface, "up").Run(); err != nil {
			log.Printf("hostapd: failed to bring %s back up: %v", iface, err)
		}
	}
	for _, cand := range []string{"wlan0", "wlan1"} {
		if cand != "" && cand != iface && interfaceExists(cand) {
			_ = runNMCli("device", "set", cand, "managed", "yes")
		}
	}
	nsm.hostapdActive = false
	return nil
}

func hostapdConfigLines(iface, band string, channel int, supportsVHT bool, country string) ([]string, error) {
	if channel == 0 {
		if band == "a" {
			channel = 36
		} else {
			channel = 1
		}
	}
	hwMode := "g"
	if band == "a" {
		hwMode = "a"
	}
	country = strings.ToUpper(country)
	htCap := ht40Capability(channel, hwMode)
	lines := []string{
		"interface=" + iface,
		"driver=nl80211",
		"ssid=bushnet",
		"hw_mode=" + hwMode,
		"channel=" + strconv.Itoa(channel),
		"auth_algs=1",
		"wmm_enabled=1",
		"ieee80211d=1",
		"country_code=" + country,
		"ignore_broadcast_ssid=0",
		"wpa=2",
		"wpa_passphrase=feathers",
		"wpa_key_mgmt=WPA-PSK",
		"rsn_pairwise=CCMP",
	}
	if htCap != "" {
		lines = append(lines, "ht_capab="+htCap)
	}
	if hwMode == "a" {
		lines = append(lines, "ieee80211n=1")
		if supportsVHT {
			lines = append(lines, "ieee80211ac=1")
			if idx, ok := vhtCenterFrequencyIndex(channel); ok {
				lines = append(lines,
					"vht_oper_chwidth=1",
					"vht_oper_centr_freq_seg0_idx="+strconv.Itoa(idx),
				)
			} else {
				lines = append(lines, "vht_oper_chwidth=0")
			}
			lines = append(lines, "vht_capab=[SHORT-GI-80]")
		} else {
			if !strings.Contains(htCap, "HT40") {
				lines = append(lines, "ht_capab=[HT40+]")
			}
		}
	} else {
		lines = append(lines,
			"ieee80211n=1",
		)
		if !strings.Contains(htCap, "HT40") {
			lines = append(lines, "ht_capab=[HT40+]")
		}
	}
	return lines, nil
}

func detectRegulatoryCountry() string {
	out, err := exec.Command("iw", "reg", "get").CombinedOutput()
	if err != nil {
		log.Printf("hostapd: failed to detect regulatory country: %v, output: %s", err, out)
		return "00"
	}
	for _, raw := range strings.Split(string(out), "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(strings.ToLower(line), "country ") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				code := strings.TrimSuffix(fields[1], ":")
				if len(code) >= 2 {
					return strings.ToUpper(code[:2])
				}
			}
		}
	}
	return "00"
}

func vhtCenterFrequencyIndex(channel int) (int, bool) {
	switch channel {
	case 36, 40, 44, 48:
		return 42, true
	case 52, 56, 60, 64:
		return 58, true
	case 100, 104, 108, 112:
		return 106, true
	case 116, 120, 124, 128:
		return 122, true
	case 132, 136, 140, 144:
		return 138, true
	case 149, 153, 157, 161:
		return 155, true
	default:
		return 0, false
	}
}

func wifiInterfaces() ([]string, error) {
	out, err := exec.Command("nmcli", "--terse", "--fields", "DEVICE,TYPE", "device", "status").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to query wifi interfaces: %w, output: %s", err, out)
	}
	seen := map[string]struct{}{}
	var interfaces []string
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) < 2 {
			continue
		}
		device := strings.TrimSpace(parts[0])
		typ := strings.TrimSpace(parts[1])
		if typ != "wifi" || device == "" {
			continue
		}
		if _, ok := seen[device]; ok {
			continue
		}
		if !interfaceExists(device) {
			continue
		}
		seen[device] = struct{}{}
		interfaces = append(interfaces, device)
	}
	if len(interfaces) == 0 {
		fallbackSeen := map[string]struct{}{}
		ifaces, err := net.Interfaces()
		if err != nil {
			return nil, err
		}
		for _, iface := range ifaces {
			if strings.HasPrefix(iface.Name, "wl") {
				if _, ok := fallbackSeen[iface.Name]; ok {
					continue
				}
				fallbackSeen[iface.Name] = struct{}{}
				interfaces = append(interfaces, iface.Name)
			}
		}
	}
	return interfaces, nil
}

func interfaceExists(name string) bool {
	if name == "" {
		return false
	}
	_, err := net.InterfaceByName(name)
	return err == nil
}

func primaryWifiInterface() string {
	if interfaceExists("wlan1") {
		return "wlan1"
	}
	if interfaceExists("wlan0") {
		return "wlan0"
	}
	return ""
}

// If wifi dongle (wlan1) is present, prefer that for client connections.
func preferredClientInterface() string {
	return primaryWifiInterface()
}

func selectHotspotRadioConfig(iface string) (string, int, bool) {
	if iface == "" {
		return "", 0, false
	}

	phyLines, err := wifiPhyInfo(iface)
	if err != nil {
		log.Printf("failed to load phy info for %s: %v", iface, err)
		return "bg", 0, false
	}

	channel, restricted := firstAvailable5GHzChannel(phyLines)
	if channel > 0 {
		msg := fmt.Sprintf("Using 5GHz band on %s with channel %d", iface, channel)
		if restricted {
			msg += " (requires regulatory update)"
		}
		log.Println(msg)
		supportsVHT := deviceSupportsVHT80(phyLines) && channelSupportsVHT80(channel)
		if supportsVHT {
			log.Printf("Hotspot interface %s advertises support for 80MHz bandwidth", iface)
		} else {
			log.Printf("Hotspot interface %s limited to 40MHz HT on channel %d", iface, channel)
		}
		return "a", channel, supportsVHT
	}
	return "bg", 0, false
}

func wifiPhyInfo(iface string) ([]string, error) {
	phy, err := wifiPhyName(iface)
	if err != nil {
		return nil, err
	}
	out, err := exec.Command("iw", "phy", phy, "info").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to read iw phy info: %w, output: %s", err, out)
	}
	return strings.Split(string(out), "\n"), nil
}

func firstAvailable5GHzChannel(lines []string) (int, bool) {
	return findFirst5GHzChannel(lines, false)
}

func findFirst5GHzChannel(lines []string, allowRestricted bool) (int, bool) {
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "*") {
			continue
		}
		lineLower := strings.ToLower(line)
		if strings.Contains(line, "(disabled)") || strings.Contains(lineLower, "radar") {
			continue
		}
		restricted := strings.Contains(lineLower, "no ir")
		if restricted && !allowRestricted {
			continue
		}
		if !strings.Contains(line, "MHz") {
			continue
		}
		freqParts := strings.Split(line, "MHz")
		if len(freqParts) == 0 {
			continue
		}
		freqStr := strings.TrimSpace(strings.TrimPrefix(freqParts[0], "*"))
		freqFields := strings.Fields(freqStr)
		if len(freqFields) == 0 {
			continue
		}
		freq, err := strconv.Atoi(freqFields[0])
		if err != nil || freq < 4900 {
			continue
		}
		chanStart := strings.Index(line, "[")
		chanEnd := strings.Index(line, "]")
		if chanStart == -1 || chanEnd == -1 || chanEnd <= chanStart+1 {
			continue
		}
		channel, err := strconv.Atoi(line[chanStart+1 : chanEnd])
		if err != nil {
			continue
		}
		return channel, restricted
	}
	if !allowRestricted {
		return findFirst5GHzChannel(lines, true)
	}
	return 0, false
}

func deviceSupportsVHT80(lines []string) bool {
	for _, line := range lines {
		lower := strings.ToLower(strings.TrimSpace(line))
		if strings.Contains(lower, "supported") && (strings.Contains(lower, "80 mhz") || strings.Contains(lower, "80mhz") || strings.Contains(lower, "80+80")) {
			return true
		}
	}
	return false
}

func channelSupportsVHT80(channel int) bool {
	switch channel {
	case 36, 40, 44, 48,
		52, 56, 60, 64,
		100, 104, 108, 112,
		116, 120, 124, 128,
		132, 136, 140, 144,
		149, 153, 157, 161:
		return true
	default:
		return false
	}
}

func ht40Capability(channel int, hwMode string) string {
	if hwMode == "a" {
		switch channel {
		case 36, 44, 52, 60, 100, 108, 116, 124, 132, 140, 149, 157:
			return "[HT40+]"
		case 40, 48, 56, 64, 104, 112, 120, 128, 136, 144, 153, 161:
			return "[HT40-]"
		default:
			return "[HT40+]"
		}
	}
	if channel == 0 {
		return "[HT40+]"
	}
	if channel <= 7 {
		return "[HT40+]"
	}
	return "[HT40-]"
}

func wifiPhyName(iface string) (string, error) {
	out, err := exec.Command("iw", "dev", iface, "info").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to query iw dev info: %w, output: %s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "wiphy ") {
			numStr := strings.TrimSpace(strings.TrimPrefix(line, "wiphy "))
			if numStr == "" {
				break
			}
			if _, err := strconv.Atoi(numStr); err == nil {
				return "phy" + numStr, nil
			}
		}
	}
	return "", fmt.Errorf("failed to detect wiphy for interface %s", iface)
}

func (nsm *networkStateMachine) setupWifi() error {
	// Deactivate hotspot if it is active, this will enable the wifi again.
	if nsm.hostapdActive {
		if err := nsm.stopHostapdHotspot(); err != nil {
			log.Printf("failed to stop hostapd hotspot: %v", err)
		}
	}
	out, err := exec.Command("nmcli", "-t", "-f", "NAME,STATE", "connection", "show", "--active").CombinedOutput()
	if err != nil {
		return fmt.Errorf("error executing nmcli: %w, output: %s", err, string(out))
	}

	log.Println("Stopping dnsmasq")
	if err := exec.Command("systemctl", "stop", "dnsmasq").Run(); err != nil {
		return err
	}

	if strings.Contains(string(out), bushnetHotspot) {
		return runNMCli("connection", "down", bushnetHotspot)
	}

	log.Println("Turn wifi radio on.")
	if err := runNMCli("radio", "wifi", "on"); err != nil {
		return err
	}
	nsm.ensureWifiDevicesManaged()
	return nil
}

func (nsm *networkStateMachine) connectWifiNetwork(ssid string) error {
	ssid = strings.TrimSpace(ssid)
	if ssid == "" {
		return fmt.Errorf("ssid cannot be empty")
	}
	if strings.EqualFold(ssid, "bushnet") || strings.EqualFold(ssid, bushnetHotspot) {
		return fmt.Errorf("refusing to connect to hotspot profile %s", ssid)
	}

	if err := nsm.setupWifi(); err != nil {
		return fmt.Errorf("failed to prepare wifi for connection: %w", err)
	}

	// Ensure NetworkManager will manage any interface we previously earmarked for the hotspot.
	if iface := nsm.hotspotInterface; iface != "" {
		if err := runNMCli("device", "set", iface, "managed", "yes"); err != nil {
			log.Printf("failed to return %s to NetworkManager management: %v", iface, err)
		}
	}

	// Ensure this profile is not pinned; we'll steer ephemerally.
	if prev, cleared, err := clearConnectionProperty(ssid, "connection.interface-name"); err != nil {
		log.Printf("failed to clear interface-name binding for %s: %v", ssid, err)
	} else if cleared {
		log.Printf("Cleared interface pin for %s (was '%s')", ssid, prev)
	}
	if prev, cleared, err := clearConnectionProperty(ssid, "802-11-wireless.mac-address"); err != nil {
		log.Printf("failed to clear mac-address binding for %s: %v", ssid, err)
	} else if cleared {
		log.Printf("Cleared mac-address pin for %s (was '%s')", ssid, prev)
	}
	if prev, cleared, err := clearConnectionProperty(ssid, "802-11-wireless.cloned-mac-address"); err != nil {
		log.Printf("failed to clear cloned-mac-address binding for %s: %v", ssid, err)
	} else if cleared {
		log.Printf("Cleared cloned-mac-address pin for %s (was '%s')", ssid, prev)
	}
	target := preferredClientInterface()
	nsm.hotspotFallback = true

	args := []string{"connection", "up", ssid}
	if target != "" && interfaceExists(target) {
		args = append(args, "ifname", target)
	}

	if err := runNMCli(args...); err != nil {
		return fmt.Errorf("failed to bring connection %s up: %w", ssid, err)
	}
	return nil
}

func getMinutesSinceHumanInteraction() (uint8, error) {
	dbusName := "org.cacophony.ATtiny"
	dbusPath := "/org/cacophony/ATtiny"

	conn, err := dbus.SystemBus()
	if err != nil {
		return 0, err
	}
	obj := conn.Object(dbusName, dbus.ObjectPath(dbusPath))
	var minutes uint8
	call := obj.Call(dbusName+".MinutesSinceHumanInteraction", 0)
	if call.Err == nil {
		if err := call.Store(&minutes); err != nil {
			return 0, err
		}
		return minutes, nil
	}
	return 0, call.Err
}
