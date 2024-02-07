package netmanagerclient

import (
	"errors"
	"fmt"
	"log"
	"os/exec"
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
	InUse   bool
}

func ScanWiFiNetworks() ([]WiFiNetwork, error) {
	out, err := exec.Command("nmcli", "device", "wifi", "rescan").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to rescan wifi: %v, output: %s", err, out)
	}

	//TODO do we need to add '--escape no' to the nmcli command?
	out, err = exec.Command("nmcli", "--terse", "--fields", "IN-USE,SIGNAL,SSID", "device", "wifi", "list").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to list wifi networks: %v, output: %s", err, out)
	}

	lines := strings.Split(string(out), "\n")

	var networks []WiFiNetwork
	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.Split(line, ":")

		if len(parts) >= 3 {
			// The SSID may contain ':' so join all parts beyond the second with ':'
			ssid := strings.Join(parts[2:], ":")

			networks = append(networks, WiFiNetwork{
				SSID:    ssid,
				Quality: parts[1],
				InUse:   parts[0] == "*",
			})
		} else {
			return nil, fmt.Errorf("failed to parse line: %s", line)
		}
	}

	return networks, nil
}

func ListUserSavedWifiNetworks() ([]WiFiNetwork, error) {
	networks, err := ListSavedWifiNetworks()
	if err != nil {
		return nil, err
	}
	userNetworks := []WiFiNetwork{}
	for _, netowrk := range networks {
		if checkIfBushnetNetwork(netowrk.SSID) == nil {
			userNetworks = append(userNetworks, netowrk)
		}
	}
	return userNetworks, nil
}

func ListSavedWifiNetworks() ([]WiFiNetwork, error) {
	out, err := exec.Command("nmcli", "--terse", "--escape", "no", "--fields", "TYPE,NAME", "connection", "show").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to list saved networks: %v, output: %s", err, out)
	}

	lines := strings.Split(string(out), "\n")

	var networks []WiFiNetwork
	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.Split(line, ":")

		if len(parts) >= 2 {
			connType := parts[0]
			// The SSID may contain ':' so join all parts beyond the first with ':'
			connSSID := strings.Join(parts[1:], ":")
			if connType == "802-11-wireless" {
				networks = append(networks, WiFiNetwork{SSID: connSSID})
			}

		} else {
			return nil, fmt.Errorf("failed to parse line: %s", line)
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
	ssid = strings.ToLower(ssid)
	if ssid == "bushnet" || ssid == "bushnethotspot" {
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

func AddWifiNetwork(ssid, psk string) error {
	if err := checkIfNetworkExists(ssid); err != nil {
		return err
	}
	if len(psk) < 8 {
		return ErrPSKTooShort
	}
	if err := checkIfBushnetNetwork(ssid); err != nil {
		return err
	}
	out, err := exec.Command(
		"nmcli", "connection", "add",
		"connection.type", "802-11-wireless",
		"wifi-sec.key-mgmt", "wpa-psk",
		"connection.id", ssid,
		"ipv4.route-metric", "10", // To make wifi preferable over the USB (modem) connection
		"ipv6.route-metric", "10",
		"wifi.ssid", ssid,
		"wifi-sec.psk", psk).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to add network: %v, output: %s", err, out)
	}
	return nil
}

func RemoveWifiNetwork(ssid string) error {
	if err := checkIfBushnetNetwork(ssid); err != nil {
		return err
	}
	out, err := exec.Command("nmcli", "connection", "delete", ssid).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to remove network: %v, output: %s", err, out)
	}
	return nil
}
