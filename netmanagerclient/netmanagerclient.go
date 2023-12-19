package managementdclient

import (
	"errors"
	"fmt"
	"log"

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

// AddNetwork will add a new wifi network.
// After adding a new network apply ReconfigureWifi() to load it.
func AddNetwork(ssid string, password string) error {
	//TODO Is a dbus call needed for this?
	_, err := eventsDbusCall("AddNetwork", ssid, password)
	return err
}

// RemoveNetwork will remove a wifi network.
// After removing a network apply ReconfigureWifi() to unload it.
func RemoveNetwork(ssid string) error {
	//TODO Is a dbus call needed for this?
	_, err := eventsDbusCall("RemoveNetwork", ssid)
	return err
}

// EnableWifi will enable the wifi.
// If the wifi is already enabled it will return unless force is true,
// then it will start up the wifi again.
func EnableWifi(force bool) error {
	_, err := eventsDbusCall("EnableWifi", force)
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
