package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	netmanagerclient "github.com/TheCacophonyProject/rpi-net-manager/netmanagerclient"
)

type networkHandler struct {
	state              netmanagerclient.NetworkState
	keepHotspotOnTimer time.Timer
	keepHotspotOnUntil time.Time // This is used to keep track of how much time is left in the timer, as you can not read that value from the timer.
	busy               bool
}

// TODO Add state wifi running but not connected.
// checkState will check if the network state need to be updated.
func (nh *networkHandler) checkState() {
	log.Println("Checking the network state")
	switch nh.state {
	case netmanagerclient.NS_WIFI:
		// If state is on wifi, check if still connected and if not start up the hotspot.
		connected, err := checkIsConnectedToNetwork()
		if err != nil {
			log.Println(err)
			return
		}
		if !connected {
			log.Println("Wifi disconnected, will try to connect again but will fall back to hotspot.")
			err := nh.setupWifiWithRollback()
			if err != nil {
				log.Println(err)
			}
		}
	case netmanagerclient.NS_HOTSPOT:
		// Nothing to do if in hotspot mode.
	case netmanagerclient.NS_WIFI_SETUP:
		// Nothing to do if wifi is being setup.
	case netmanagerclient.NS_HOTSPOT_SETUP:
		// Nothing to do if hotspot is being setup.
	default:
		log.Println("Unhandled network state:", nh.state)
	}
}

func (nh *networkHandler) keepHotspotOnFor(keepOnFor time.Duration) {
	newKeepOnUntil := time.Now().Add(keepOnFor)
	if newKeepOnUntil.After(nh.keepHotspotOnUntil) {
		log.Println("Keep hotspot on for", keepOnFor)
		nh.keepHotspotOnUntil = newKeepOnUntil
		nh.keepHotspotOnTimer.Reset(keepOnFor)
	} else {
		log.Printf("Keep hotspot on for %s, but already on for %s", keepOnFor, time.Until(nh.keepHotspotOnUntil))
	}
}

func (nh *networkHandler) setState(ns netmanagerclient.NetworkState) {
	//TODO thread safety
	if nh.state != ns {
		log.Printf("State changed from %s to %s", nh.state, ns)
		nh.state = ns
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

func checkIsConnectedToNetwork() (bool, error) {
	out, err := exec.Command("nmcli", "--fields", "DEVICE,STATE,CONNECTION", "--terse", "--color", "no", "device", "status").CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("error executing nmcli: %w", err)
	}

	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		parts := strings.Split(line, ":")
		if len(parts) < 2 {
			continue
		}
		device := parts[0]
		state := parts[1]
		connection := strings.Join(parts[2:], ":")
		if device == "wlan0" {
			if connection == bushnetHotspot {
				return false, nil
			}
			return state == "connected", nil
		}
	}
	return false, fmt.Errorf("could not find wlan0 network")
}

// TODO this can be improved by waiting less if no saved wifi network is available to connect to.
func waitAndCheckIfConnectedToNetwork() (bool, error) {
	for i := 0; i < 10; i++ {
		connected, err := checkIsConnectedToNetwork()
		if err != nil {
			return false, err
		}
		if connected {
			return true, nil
		}
		time.Sleep(time.Second)
	}
	return false, nil
}

func (nh *networkHandler) setupWifiWithRollback() error {
	if nh.busy {
		return fmt.Errorf("busy")
	}
	if nh.state != netmanagerclient.NS_WIFI {
		if err := nh.setupWifi(); err != nil {
			return err
		}
	}

	nh.busy = true
	defer func() { nh.busy = false }()

	log.Println("WiFi network is up, checking that device can connect to a network.")
	connected, err := checkIsConnectedToNetwork()
	if err != nil {
		return err
	}
	if connected {
		nh.setState(netmanagerclient.NS_WIFI)
		log.Println("connected")
		return nil
	}
	nh.setState(netmanagerclient.NS_WIFI_SETUP)
	connected, err = waitAndCheckIfConnectedToNetwork()
	if err != nil {
		return err
	}
	if connected {
		nh.setState(netmanagerclient.NS_WIFI)
	} else {
		log.Println("Failed to connect to wifi. Starting up hotspot.")
		nh.busy = false
		return nh.setupHotspot()
	}
	return nil
}

func (nh *networkHandler) setupHotspot() error {
	if nh.busy {
		return fmt.Errorf("busy")
	}
	nh.busy = true
	defer func() { nh.busy = false }()
	nh.setState(netmanagerclient.NS_HOTSPOT_SETUP)

	log.Println("Setting up network for hosting a hotspot.")
	networks, err := netmanagerclient.ListSavedWifiNetworks()
	if err != nil {
		return err
	}
	hotspotConfigured := false
	for _, network := range networks {
		if network.SSID == bushnetHotspot {
			hotspotConfigured = true
		}
	}
	if !hotspotConfigured {
		log.Printf("'%s' not found, creating.", bushnetHotspot)
		err = runNMCli(
			"connection", "add", "type", "wifi", "ifname", "wlan0", "con-name", bushnetHotspot,
			"autoconnect", "no", "ssid", "bushnet",
			"802-11-wireless.mode", "ap",
			"802-11-wireless.band", "bg",
			"ipv4.method", "manual", // Using 'manual' instead of 'shared' so can configure dnsmasq to not share the internet connection of the modem to connected devices.
			"wifi-sec.key-mgmt", "wpa-psk",
			"wifi-sec.psk", "feathers",
			"ipv4.addresses", router_ip+"/24")
		if err != nil {
			return err
		}
	}

	err = runNMCli("connection", "up", bushnetHotspot)
	if err != nil {
		return err
	}

	if err := createDNSConfig(router_ip, "192.168.4.2,192.168.4.20"); err != nil {
		return err
	}

	log.Printf("Starting DNS...")
	if err := exec.Command("systemctl", "restart", "dnsmasq").Run(); err != nil {
		return err
	}

	nh.setState(netmanagerclient.NS_HOTSPOT)
	return nil
}

const router_ip = "192.168.4.1"

func createDNSConfig(router_ip string, ip_range string) error {
	file_name := "/etc/dnsmasq.conf"
	config_lines := []string{
		"interface=wlan0",
		"dhcp-range=" + ip_range + ",12h",
		"domain=wlan",
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

func (nh *networkHandler) setupWifi() error {
	if nh.busy {
		return fmt.Errorf("busy")
	}
	nh.busy = true
	defer func() { nh.busy = false }()
	nh.setState(netmanagerclient.NS_WIFI_SETUP)

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
	return nil
}
