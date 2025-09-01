package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	netmanagerclient "github.com/TheCacophonyProject/rpi-net-manager/netmanagerclient"
	"github.com/godbus/dbus/v5"
)

type networkStateMachine struct {
	mux                  sync.Mutex
	state                netmanagerclient.NetworkState
	connName             string
	wifiScanConnectTimer *time.Timer
	wifiScanTimer        *time.Timer
	hotspotTimer         *time.Timer
	keepHotspotOnUntil   time.Time
	NetworkUpdateChannel chan struct{}
	hotspotFallback      bool
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
	logBssid()

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
		resetTimer(nsm.wifiScanTimer, 10*time.Second)
		// Reset timer for wifi to scan and connect to a network.
		if oldState != netmanagerclient.NS_WIFI_CONNECTING {
			resetTimer(nsm.wifiScanConnectTimer, 10*time.Minute)
		}

	case netmanagerclient.NS_HOTSPOT_RUNNING:
		// Reset timer for hotspot when it has started up.
		resetTimer(nsm.hotspotTimer, 5*time.Minute)

	case netmanagerclient.NS_WIFI_CONNECTED:
		// Set auth retries to 2 in case it was set to 1 previously.
		if err := runNMCli("connection", "modify", newConName, "connection.auth-retries", "2"); err != nil {
			log.Printf("failed to set auth-retries to 2, '%s'", err)
		}
	}
	return nil
}

// Utility function to safely reset a timer
func resetTimer(timer *time.Timer, duration time.Duration) {
	if timer == nil {
		timer = time.NewTimer(duration)
	}
	if !(timer).Stop() {
		select {
		case <-(*timer).C: // Drain the channel if needed
		default:
		}
	}
	timer.Reset(duration)
}

func (nsm *networkStateMachine) runStateMachine() error {
	log.Println("Starting Network Manager state machine")
	if err := runNMCli("radio", "wifi", "on"); err != nil {
		return err
	}

	// Timeout flags
	wifiScanConnectTimeout := false
	wifiScanTimeout := false
	hotspotTimeout := false

	nsm.mux.Lock()
	defer nsm.mux.Unlock()

	for {
		// Look at the network setup to determine what network state the device is in.
		newState, conName, err := detectState()
		if err != nil {
			return err
		}
		// Handle state transitions, this will reset appropriate timers if needed.
		err = nsm.handleStateTransition(newState, conName)
		if err != nil {
			return err
		}

		// Update the state
		switch nsm.state {
		case netmanagerclient.NS_WIFI_OFF:
			// Turn wifi back on if button is pressed, this will get handled elsewhere.
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
		return "", fmt.Errorf("no active AP found")
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

func logBssid() {
	bssidOutRaw, err := exec.Command("nmcli", "--terse", "--fields", "AP.BSSID,AP.IN-USE", "device", "show", "wlan0").CombinedOutput()
	if err != nil {
		log.Printf("failed to run nmcli: %v, output: %s", err, bssidOutRaw)
		return
	}
	bssid, err := getActiveBSSIDFromOutput(string(bssidOutRaw))
	if err != nil {
		log.Printf("failed to get active bssid: %v, output: %s", err, bssidOutRaw)
		return
	}
	log.Info("Active BSSID:", bssid)
}

func (nsm *networkStateMachine) keepHotspotOnFor(keepOnFor time.Duration) {
	newKeepOnUntil := time.Now().Add(keepOnFor)
	if newKeepOnUntil.After(nsm.keepHotspotOnUntil) {
		log.Println("Keep hotspot on for", keepOnFor)
		nsm.keepHotspotOnUntil = newKeepOnUntil
		resetTimer(nsm.hotspotTimer, keepOnFor)
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

	log.Println("Setting up network for hosting a hotspot.")
	hotspotConfig := map[string]string{
		"connection.type":              "802-11-wireless",
		"ifname":                       "wlan0",
		"autoconnect":                  "no",
		"ssid":                         "bushnet",
		"802-11-wireless.mode":         "ap",
		"802-11-wireless.band":         "bg",
		"ipv4.method":                  "manual", // Using 'manual' instead of 'shared' so can configure dnsmasq to not share the internet connection of the modem to connected devices.
		"wifi-sec.key-mgmt":            "wpa-psk",
		"wifi-sec.psk":                 "feathers",
		"ipv4.addresses":               router_ip + "/24",
		"802-11-wireless-security.pmf": "disable", // Android has issues with PMF
	}

	if err := netmanagerclient.ModifyNetworkConfig(bushnetHotspot, hotspotConfig); err != nil {
		return err
	}

	log.Println("Starting hotspot...")
	if err := runNMCli("connection", "up", bushnetHotspot); err != nil {
		return err
	}

	if err := createDNSConfig("192.168.4.2,192.168.4.20"); err != nil {
		return err
	}

	log.Printf("Starting DNS...")
	if err := exec.Command("systemctl", "restart", "dnsmasq").Run(); err != nil {
		return err
	}
	return nil
}

func detectState() (netmanagerclient.NetworkState, string, error) {
	out, err := exec.Command("nmcli", "radio", "wifi").CombinedOutput()
	if err != nil {
		return netmanagerclient.NS_ERROR, "", fmt.Errorf("error getting wifi radio state %s, err: %s", out, err)
	}
	radioState := strings.TrimSpace(string(out))
	if radioState == "disabled" {
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

const router_ip = "192.168.4.1"

func createDNSConfig(ip_range string) error {
	file_name := "/etc/dnsmasq.conf"
	config_lines := []string{
		"interface=wlan0",
		"dhcp-range=" + ip_range + ",12h",
		"domain=wlan",
		"server=1.1.1.1",
		"server=8.8.8.8",
	}
	return createConfigFile(file_name, config_lines)
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

func (nsm *networkStateMachine) setupWifi() error {
	// Deactivate hotspot if it is active, this will enable the wifi again.
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
