package netmanagerclient

import (
	"errors"
	"fmt"
	"log"
	"os/exec"
	"regexp"
	"strings"

	"github.com/godbus/dbus/v5"
)

type NetworkState string

const (
	NS_WIFI          NetworkState = "WIFI"
	NS_WIFI_SETUP    NetworkState = "WIFI_SETUP"
	NS_HOTSPOT       NetworkState = "HOTSPOT"
	NS_HOTSPOT_SETUP NetworkState = "HOTSPOT_SETUP"
)

func stringToNetworkState(s string) (NetworkState, error) {
	switch s {
	case string(NS_WIFI):
		return NS_WIFI, nil
	case string(NS_WIFI_SETUP):
		return NS_WIFI_SETUP, nil
	case string(NS_HOTSPOT):
		return NS_HOTSPOT, nil
	case string(NS_HOTSPOT_SETUP):
		return NS_HOTSPOT_SETUP, nil
	default:
		return "", errors.New("invalid network state")
	}
}

const (
	DbusInterface = "org.cacophony.RPiNetManager"
	DbusPath      = "/org/cacophony/RPiNetManager"
)

func KeepHotspotOnFor(seconds int) error {
	_, err := eventsDbusCall("KeepHotspotOnFor", seconds)
	return err
}

// ReadState will read the current state of the network.
func ReadState() (NetworkState, error) {
	data, err := eventsDbusCall("ReadState")
	if err != nil {
		return "", err
	}
	if len(data) != 1 {
		return "", errors.New("error getting state")
	}
	stateStr, ok := data[0].(string)
	if !ok {
		return "", errors.New("error reading state")
	}
	return stringToNetworkState(stateStr)
}

// ReconfigureWifi will reconfigure the wifi network.
// Call this after adding a new wifi network for it to be loaded.
func ReconfigureWifi() error {
	_, err := eventsDbusCall("ReconfigureWifi")
	return err
}

// EnableWifi will enable the wifi.
// If the wifi is already enabled it will return unless force is true,
// then it will start up the wifi again.
func EnableWifi(force bool) error {
	log.Println("Making call to EnableWifi")
	_, err := eventsDbusCall("EnableWifi", force)
	return err
}

func CheckState() error {
	_, err := eventsDbusCall("CheckState")
	return err
}

// EnableHotspot will enable the hotspot.
// If the hotspot is already enabled it will return unless force is true,
// then it will start up the hotspot again.
func EnableHotspot(force bool) error {
	_, err := eventsDbusCall("EnableHotspot", force)
	return err
}

func eventsDbusCall(method string, params ...interface{}) ([]interface{}, error) {
	conn, err := dbus.SystemBus()
	if err != nil {
		return nil, err
	}
	obj := conn.Object(DbusInterface, DbusPath)
	call := obj.Call(DbusInterface+"."+method, 0, params...)
	return call.Body, call.Err
}

// GetStateChanges will start listening for state changes.
func GetStateChanges() (chan NetworkState, chan<- struct{}, error) {
	stateChan := make(chan NetworkState, 10)
	done := make(chan struct{})

	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to connect to System Bus: %v", err)
	}

	// Add a match rule to listen for our specific signal
	matchRule := dbus.WithMatchInterface(DbusInterface)
	conn.AddMatchSignal(matchRule)

	// Channel to receive signals
	c := make(chan *dbus.Signal, 10)
	conn.Signal(c)

	go func() {
		defer close(stateChan)
		defer conn.Close()

		for {
			select {
			case v := <-c:
				if len(v.Body) > 0 {
					str, ok := v.Body[0].(string)
					if !ok {
						log.Println("Signal does not contain a string in Body[0]")
						continue
					}
					state, err := stringToNetworkState(str)
					if err != nil {
						log.Println("Failed to parse state:", err)
						continue
					}
					stateChan <- state
				}
			case <-done:
				log.Println("Stopping signal listener")
				return
			}
		}
	}()

	return stateChan, done, nil
}

type WiFiNetwork struct {
	SSID    string
	Quality string
	ID      string
}

func ScanWiFiNetworks() ([]WiFiNetwork, error) {
	cmd := exec.Command("sudo", "iwlist", "wlan0", "scan")
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	networks := []WiFiNetwork{}
	lines := strings.Split(string(output), "\n")
	network := WiFiNetwork{}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Cell") {
			network = WiFiNetwork{}
		}
		if strings.HasPrefix(line, "ESSID") {
			matches := regexp.MustCompile(`ESSID:"(.*)"`).FindStringSubmatch(line)
			if len(matches) == 2 {
				network.SSID = matches[1]
				if network.SSID != "" {
					networks = append(networks, network)
				}
			} else {
				log.Println(matches)
				log.Println("Failed to parse SSID:", line)
			}
		}
		if strings.HasPrefix(line, "Quality") {
			matches := regexp.MustCompile(`Quality=([^ ]+)`).FindStringSubmatch(line)
			if len(matches) == 2 {
				network.Quality = matches[1]
			} else {
				log.Println("Failed to parse Quality:", line)
			}
		}
	}
	return networks, nil
}

func ListSavedWifiNetworks() ([]WiFiNetwork, error) {
	cmd := exec.Command("wpa_cli", "-i", "wlan0", "list_networks")
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	networks := []WiFiNetwork{}
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		bits := strings.Split(line, "\t")
		if len(bits) > 1 {
			networks = append(networks, WiFiNetwork{
				ID:   bits[0],
				SSID: bits[1],
			})
		}
	}
	return networks, nil
}

type InputError struct {
	Message string
}

func (e InputError) Error() string {
	return fmt.Sprintf("Input Error: %s", e.Message)
}

var ErrNetworkAlreadyExists = InputError{Message: "a network with the given SSID already exists"}
var ErrPSKTooShort = InputError{Message: "the given PSK is too short, must be at least 8 characters long"}
var ErrBushnetNetwork = InputError{Message: "the given SSID is a Bushnet network so can't be modified"}

func checkIfBushnetNetwork(ssid string) error {
	if ssid == "Bushnet" || ssid == "bushnet" {
		return ErrBushnetNetwork
	}
	return nil
}

func checkIfNetworkExists(ssid string) error {
	networks, err := ListSavedWifiNetworks()
	if err != nil {
		return err
	}
	for _, network := range networks {
		if network.SSID == ssid {
			return ErrNetworkAlreadyExists
		}
	}
	return nil
}

// addWifiNetwork adds a new WiFi network with the given SSID and password.
func AddWifiNetwork(ssid, password string) error {
	// Check that there is not already a network with the given SSID
	if err := checkIfNetworkExists(ssid); err != nil {
		return err
	}

	if len(password) < 8 {
		return ErrPSKTooShort
	}

	if err := checkIfBushnetNetwork(ssid); err != nil {
		return err
	}

	// If in one of the step the network fails then make sure that it is deleted.
	networkAddedSuccess := false
	defer func() {
		if !networkAddedSuccess {
			log.Println("network fail to be added, trying to remove it")
			if err := checkIfNetworkExists(ssid); err == ErrNetworkAlreadyExists {
				if err := RemoveWifiNetwork(ssid); err != nil {
					log.Println(err)
				}
			} else {
				log.Println(err)
			}
		}
	}()

	cmd := exec.Command("wpa_cli", "-i", "wlan0", "add_network")
	networkID, err := cmd.Output()
	id := strings.TrimSpace(string(networkID))
	if err != nil {
		return err
	}

	if err := runWPACommand("wpa_cli", "-i", "wlan0", "set_network", id, "ssid", fmt.Sprintf("\"%s\"", ssid)); err != nil {
		return err
	}

	if err := runWPACommand("wpa_cli", "-i", "wlan0", "set_network", id, "psk", fmt.Sprintf("\"%s\"", password)); err != nil {
		return err
	}

	if err := runWPACommand("wpa_cli", "-i", "wlan0", "enable_network", id); err != nil {
		return err
	}

	if err := runWPACommand("wpa_cli", "-i", "wlan0", "save", "config"); err != nil {
		return err
	}
	networkAddedSuccess = true
	return nil
}

func RemoveWifiNetwork(ssid string) error {
	if err := checkIfBushnetNetwork(ssid); err != nil {
		return err
	}
	networks, err := ListSavedWifiNetworks()
	if err != nil {
		return err
	}
	id := ""
	for _, network := range networks {
		if network.SSID == ssid {
			id = network.ID
		}
	}
	if id == "" {
		log.Printf("when trying to delete network '%s' it was not found", ssid)
		return nil
	}
	if err := runWPACommand("wpa_cli", "-i", "wlan0", "remove_network", id); err != nil {
		return err
	}

	if err := runWPACommand("wpa_cli", "-i", "wlan0", "save", "config"); err != nil {
		return err
	}

	return nil
}

func runWPACommand(args ...string) error {
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("error running wpa command: '%s', output: %s", strings.Join(args, " "), string(out))
	}
	if strings.Contains(string(out), "FAIL") || strings.Contains(string(out), "exit status 255") {
		return fmt.Errorf("error running wpa command: '%s', output: %s", strings.Join(args, " "), string(out))
	}
	return nil
}
