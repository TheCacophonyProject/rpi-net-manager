package netmanagerclient

import (
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

type NetworkState string

const (
	NS_INIT             NetworkState = "Init"             // Initial state, before any network state changes
	NS_WIFI_OFF         NetworkState = "WIFI_OFF"         // WIFI Radio is off.
	NS_WIFI_SETUP       NetworkState = "WIFI_SETUP"       // WIFI is being setup.
	NS_WIFI_SCANNING    NetworkState = "WIFI_SCANNING"    // WIFI is scanning for networks to connect to.
	NS_WIFI_CONNECTING  NetworkState = "WIFI_CONNECTING"  // WIFI is trying to connect to a network.
	NS_WIFI_CONNECTED   NetworkState = "WIFI_CONNECTED"   // WIFI has connected to a network.
	NS_HOTSPOT_STARTING NetworkState = "HOTSPOT_STARTING" // Hotspot is being setup.
	NS_HOTSPOT_RUNNING  NetworkState = "HOTSPOT_RUNNING"  // Hotspot is running.
	NS_ERROR            NetworkState = "Error with network"
)

func stringToNetworkState(s string) (NetworkState, error) {
	switch s {
	case string(NS_INIT):
		return NS_INIT, nil
	case string(NS_WIFI_OFF):
		return NS_WIFI_OFF, nil
	case string(NS_WIFI_SETUP):
		return NS_WIFI_SETUP, nil
	case string(NS_WIFI_SCANNING):
		return NS_WIFI_SCANNING, nil
	case string(NS_WIFI_CONNECTING):
		return NS_WIFI_CONNECTING, nil
	case string(NS_WIFI_CONNECTED):
		return NS_WIFI_CONNECTED, nil
	case string(NS_HOTSPOT_STARTING):
		return NS_HOTSPOT_STARTING, nil
	case string(NS_HOTSPOT_RUNNING):
		return NS_HOTSPOT_RUNNING, nil
	case string(NS_ERROR):
		return NS_ERROR, nil
	default:
		return "", fmt.Errorf("invalid network state, '%s'", s)
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
// The force parameter doesn't do anything at the moment.
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
	SSID               string
	Quality            string
	ID                 string
	InUse              bool
	AuthFailed         bool
	LastConnectionTime time.Time
}

func ScanWiFiNetworks() ([]WiFiNetwork, error) {
	// TODO do we need to add '--escape no' to the nmcli command?
	out, err := exec.Command("nmcli", "--terse", "--fields", "IN-USE,SIGNAL,SSID", "device", "wifi", "list", "--rescan", "yes").CombinedOutput()
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
	for _, network := range networks {
		if checkIfBushnetNetwork(network.SSID) == nil {
			userNetworks = append(userNetworks, network)
		}
	}
	return userNetworks, nil
}

// FindNetworkBySSID searches for a network by SSID in the list of WiFi networks
func FindNetworkBySSID(ssid string) (WiFiNetwork, bool) {
	networks, err := ListUserSavedWifiNetworks()
	if err != nil {
		log.Fatalf("Error listing saved WiFi networks: %v", err)
		return WiFiNetwork{}, false
	}
	for _, network := range networks {
		if network.SSID == ssid {
			return network, true
		}
	}
	return WiFiNetwork{}, false
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
			connName := strings.Join(parts[1:], ":")
			if connType == "802-11-wireless" {
				propMap, err := getConnectionProperties(connName, []string{"connection.auth-retries", "connection.timestamp", "802-11-wireless.ssid"})
				if err != nil {
					return nil, err
				}

				sec, err := strconv.ParseInt(propMap["connection.timestamp"], 10, 64)
				lastConnectionTime := time.Time{}
				if err != nil {
					log.Printf("Failed to part time '%s'", err)
					return nil, err
				} else {
					lastConnectionTime = time.Unix(sec, 0)
				}
				// if auth-retries is 1, last time the connection failed.
				authFailed := propMap["connection.auth-retries"] == "1"

				networks = append(networks, WiFiNetwork{
					ID:                 connName,
					SSID:               propMap["802-11-wireless.ssid"],
					AuthFailed:         authFailed,
					LastConnectionTime: lastConnectionTime,
				})
			}

		} else {
			return nil, fmt.Errorf("failed to parse line: %s", line)
		}
	}

	return networks, nil
}

func getConnectionProperties(connection string, properties []string) (map[string]string, error) {
	out, err := exec.Command("nmcli", "-f", strings.Join(properties, ","), "-t", "connection", "show", connection).CombinedOutput()
	if err != nil {
		return nil, err
	}
	propMap := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) >= 2 {
			propMap[parts[0]] = strings.Join(parts[1:], ":")
		}
	}
	return propMap, nil
}

type InputError struct {
	Message string
}

func (e InputError) Error() string {
	return fmt.Sprintf("Input Error: %s", e.Message)
}

var (
	ErrNetworkAlreadyExists = InputError{Message: "a network with the given SSID already exists"}
	ErrPSKTooShort          = InputError{Message: "the given PSK is too short, must be at least 8 characters long"}
	ErrBushnetNetwork       = InputError{Message: "the given SSID is a Bushnet network so can't be modified"}
)

func checkIfBushnetNetwork(ssid string) error {
	ssid = strings.ToLower(ssid)
	if ssid == "bushnet" || ssid == "bushnethotspot" {
		return ErrBushnetNetwork
	}
	return nil
}

func CheckIfNetworkExists(id string) (bool, error) {
	networks, err := ListSavedWifiNetworks()
	if err != nil {
		return false, err
	}
	for _, network := range networks {
		if network.ID == id {
			return true, nil
		}
	}
	return false, nil
}

// Connects to an existing network
func ConnectWifiNetwork(ssid string) error {
	if err := checkIfBushnetNetwork(ssid); err != nil {
		return err
	}
	out, err := exec.Command("nmcli", "connection", "up", ssid).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to connect to network: %v, output: %s", err, out)
	}
	return nil
}

func ModifyWifiNetwork(ssid, psk string) error {
	if len(psk) < 8 {
		return ErrPSKTooShort
	}
	if err := checkIfBushnetNetwork(ssid); err != nil {
		return err
	}
	out, err := exec.Command(
		"nmcli", "connection", "modify", ssid,
		"wifi-sec.psk", psk).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to modify network: %v, output: %s", err, out)
	}
	return nil
}

func AddWifiNetwork(ssid, psk string) error {
	alreadyExists, err := CheckIfNetworkExists(ssid)
	if err != nil {
		return err
	}
	if alreadyExists {
		return ErrNetworkAlreadyExists
	}
	if len(psk) < 8 {
		return ErrPSKTooShort
	}
	if err := checkIfBushnetNetwork(ssid); err != nil {
		return err
	}

	c := map[string]string{
		"connection.type":         "802-11-wireless",
		"connection.auth-retries": "2",
		"wifi-sec.key-mgmt":       "wpa-psk",
		"connection.id":           ssid,
		"ipv4.route-metric":       "10", // To make wifi preferable over the USB (modem) connection
		"ipv6.route-metric":       "10",
		"wifi.ssid":               ssid,
		"wifi-sec.psk":            psk,
	}
	//"connection.autoconnect-retries", "2", //TODO look into this option more.

	if err := ModifyNetworkConfig(ssid, c); err != nil {
		return fmt.Errorf("failed to add network: %v", err)
	}
	return nil
}

func RemoveWifiNetwork(ssid string, disconnect bool, startHotspot bool) error {
	if err := checkIfBushnetNetwork(ssid); err != nil {
		return err
	}
	out, err := exec.Command("nmcli", "connection", "delete", ssid).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to remove network: %v, output: %s", err, out)
	}

	if disconnect {
		out, err = exec.Command("nmcli", "connection", "down", ssid).CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed to disconnect network: %v, output: %s", err, out)
		}
	}

	if startHotspot {
		EnableHotspot(true)
	}
	return nil
}

func DisconnectWifiNetwork(ssid string, startHotspot bool) error {
	if err := checkIfBushnetNetwork(ssid); err != nil {
		return err
	}
	out, err := exec.Command("nmcli", "connection", "down", ssid).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to forget network: %v, output: %s", err, out)
	}

	if startHotspot {
		EnableHotspot(true)
	}
	return nil
}

// ModifyNetworkConfig will check if a networks exists, create it if not, then set the given config values to it.
func ModifyNetworkConfig(id string, c map[string]string) error {
	exists, err := CheckIfNetworkExists(id)
	if err != nil {
		return err
	}
	if exists {
		for k, v := range c {
			out, err := exec.Command("nmcli", "connection", "modify", id, k, v).CombinedOutput()
			if err != nil {
				return fmt.Errorf("failed to modify network: %v, output: %s", err, out)
			}
		}
		return nil
	}

	configCommands := []string{"connection", "add", "connection.id", id}

	// Add connection.type first or else nmcli could fail depending on the order.
	val, typeExists := c["connection.type"]
	if typeExists {
		configCommands = append(configCommands, "connection.type", val)
	}

	for k, v := range c {
		if k == "connection.type" {
			continue
		}
		configCommands = append(configCommands, k, v)
	}

	out, err := exec.Command("nmcli", configCommands...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to create network: %v, output: %s", err, out)
	}
	return nil
}
