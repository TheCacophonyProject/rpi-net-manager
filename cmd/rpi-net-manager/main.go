package main

import (
	"fmt"
	"os"
	"time"

	"github.com/TheCacophonyProject/go-utils/logging"
	netmanagerclient "github.com/TheCacophonyProject/rpi-net-manager/netmanagerclient"
	"github.com/alexflint/go-arg"
	"github.com/godbus/dbus/v5"
)

var version = "<not set>"
var log = logging.NewLogger("info")

type AddNetwork struct {
	SSID string `arg:"required" help:"the SSID of the network"`
	Pass string `arg:"required" help:"the password of the network"`
}
type RemoveNetwork struct {
	SSID string `arg:"required" help:"the SSID of the network"`
}
type EnableHotspot struct {
	Force bool `arg:"--force" help:"force enable hotspot"`
}
type EnableWifi struct {
	Force bool `arg:"--force" help:"force enable wifi"`
}
type ReadState struct {
	FollowUpdates bool `arg:"--follow-updates" help:"keep on reading the state as it updates instead of just once"`
}
type subcommand struct{}

type Args struct {
	Service              *subcommand    `arg:"subcommand:service" help:"start service"`
	ReadState            *ReadState     `arg:"subcommand:read-state" help:"read the state of the network"`
	SavedWifiNetworks    *subcommand    `arg:"subcommand:saved-wifi-networks" help:"show saved wifi networks"`
	AddWifiNetwork       *AddNetwork    `arg:"subcommand:add-wifi-network" help:"add a network"`
	RemoveWifiNetwork    *RemoveNetwork `arg:"subcommand:remove-wifi-network" help:"remove a network"`
	EnableWifi           *EnableWifi    `arg:"subcommand:enable-wifi" help:"enable wifi"`
	EnableHotspot        *EnableHotspot `arg:"subcommand:enable-hotspot" help:"enable hotspot"`
	ScanNetwork          *subcommand    `arg:"subcommand:scan-network" help:"show available networks"`
	ShowConnectedDevices *subcommand    `arg:"subcommand:show-connected-devices" help:"show connected devices on the hotspot //TODO"`
	ModemStatus          *subcommand    `arg:"subcommand:modem-status" help:"show modem status //TODO"`
	CheckState           *subcommand    `arg:"subcommand:check-state" help:"check if the state needs to be updated"`
	logging.LogArgs
}

func (Args) Version() string {
	return version
}

func procArgs() Args {
	args := Args{}
	_ = arg.MustParse(&args)
	return args
}

func main() {
	err := runMain()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func runMain() error {
	args := procArgs()

	log = logging.NewLogger(args.LogLevel)

	log.Printf("Running version: %s", version)
	if os.Geteuid() != 0 {
		return fmt.Errorf("rpi-net-manager must be run as root")
	}

	if args.Service != nil {
		if err := startService(); err != nil {
			return err
		}
		// Block the goroutine
		quit := make(chan struct{})
		<-quit
		return nil
	} else if args.ReadState != nil {
		return readState(args)
	} else if args.SavedWifiNetworks != nil {
		return savedWifiNetworks()
	} else if args.AddWifiNetwork != nil {
		return addWifiNetwork(args.AddWifiNetwork.SSID, args.AddWifiNetwork.Pass)
	} else if args.RemoveWifiNetwork != nil {
		return removeWifiNetwork(args.RemoveWifiNetwork.SSID)
	} else if args.EnableWifi != nil {
		return enableWifi(args)
	} else if args.EnableHotspot != nil {
		return enableHotspot(args)
	} else if args.ScanNetwork != nil {
		return scanNetwork()
	} else if args.CheckState != nil {
		return checkState()
	} else {
		return fmt.Errorf("no command given, use --help for usage")
	}
}

func startService() error {

	bushnetConfig := map[string]string{
		"connection.type":                 "802-11-wireless",
		"connection.auth-retries":         "2",
		"connection.autoconnect-priority": "10",
		"ipv4.route-metric":               "10",
		"ipv6.route-metric":               "10",
		"wifi.ssid":                       "bushnet",
		"wifi-sec.psk":                    "feathers",
		"wifi-sec.key-mgmt":               "wpa-psk",
	}
	if err := netmanagerclient.ModifyNetworkConfig("bushnet", bushnetConfig); err != nil {
		return err
	}

	bushnetConfig["wifi.ssid"] = "Bushnet"
	if err := netmanagerclient.ModifyNetworkConfig("Bushnet", bushnetConfig); err != nil {
		return err
	}

	c, done, err := makeNetworkUpdateChan()
	if err != nil {
		return err
	}
	defer close(done)

	nsm := &networkStateMachine{
		NetworkUpdateChannel: c,
		state:                netmanagerclient.NS_INIT,
		wifiScanConnectTimer: time.NewTimer(10 * time.Minute),
		wifiScanTimer:        time.NewTimer(10 * time.Second),
		hotspotTimer:         time.NewTimer(5 * time.Minute),
		hotspotFallback:      true,
	}

	if err := startDBusService(nsm); err != nil {
		return err
	}

	if err := nsm.runStateMachine(); err != nil {
		return err
	}

	return nil
}

func readState(args Args) error {
	log.Println("Reading state.")
	state, err := netmanagerclient.ReadState()
	if err != nil {
		return nil
	}
	log.Println(state)
	if args.ReadState.FollowUpdates {
		stateChan, done, err := netmanagerclient.GetStateChanges()
		defer close(done)
		if err != nil {
			return err
		}
		for state = range stateChan {
			log.Println(time.Now().Format(time.TimeOnly), state)
		}
	}
	return nil
}

func savedWifiNetworks() error {
	log.Println("Listing saved wifi networks.")
	networks, err := netmanagerclient.ListSavedWifiNetworks()
	if err != nil {
		return err
	}
	for _, network := range networks {
		log.Printf("ID: '%s', SSID: '%s', LastConnectionTime: '%s', AuthFailed: '%t'", network.ID, network.SSID, network.LastConnectionTime, network.AuthFailed)
	}
	return nil
}

func addWifiNetwork(ssid, pass string) error {
	log.Println("Adding network. SSID: ", ssid, " Pass: ", pass)
	return netmanagerclient.AddWifiNetwork(ssid, pass)
}

func removeWifiNetwork(ssid string) error {
	log.Println("Removing network. SSID: ", ssid)
	return netmanagerclient.RemoveWifiNetwork(ssid, false, false)
}

func enableWifi(args Args) error {
	log.Println("Enabling wifi.")
	return netmanagerclient.EnableWifi(args.EnableWifi.Force)
}

func enableHotspot(args Args) error {
	log.Println("Enabling hotspot.")
	return netmanagerclient.EnableHotspot(args.EnableHotspot.Force)
}

func scanNetwork() error {
	networks, err := netmanagerclient.ScanWiFiNetworks()
	if err != nil {
		return err
	}
	for _, network := range networks {
		log.Printf("SSID: '%s', Quality: '%s', InUse: '%t'", network.SSID, network.Quality, network.InUse)
	}
	return nil
}

func checkState() error {
	log.Println("Checking state.")
	err := netmanagerclient.CheckState()
	if err != nil {
		return err
	}
	return nil
}

// GetStateChanges will start listening for state changes.
func makeNetworkUpdateChan() (chan struct{}, chan<- struct{}, error) {
	stateChan := make(chan struct{}, 10)
	done := make(chan struct{})

	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to connect to System Bus: %v", err)
	}

	// Subscribe to properties changed signal for NetworkManager
	conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0,
		"type='signal',interface='org.freedesktop.DBus.Properties',"+
			"sender='org.freedesktop.NetworkManager',"+
			"path='/org/freedesktop/NetworkManager',"+
			"arg0namespace='org.freedesktop.NetworkManager'")

	c := make(chan *dbus.Signal, 10)
	conn.Signal(c)

	fmt.Println("Listening for network changes...")

	go func() {
		defer close(stateChan)
		defer conn.Close()

		for {
			select {
			case v := <-c:
				if v.Name == "org.freedesktop.DBus.Properties.PropertiesChanged" && len(v.Body) >= 3 {
					interfaceName, ok := v.Body[0].(string)
					if !ok {
						continue
					}
					// Check if the signal is for NetworkManager
					if interfaceName == "org.freedesktop.NetworkManager" {
						changes, ok := v.Body[1].(map[string]dbus.Variant)
						if !ok {
							continue
						}
						// Only handle specific changes
						if _, exists := changes["Connectivity"]; exists {
							stateChan <- struct{}{}
						}
						if _, exists := changes["ActiveConnections"]; exists {
							stateChan <- struct{}{}
						}
					}
				}
			case <-done:
				log.Println("Stopping signal listener")
				return
			}
		}
	}()
	return stateChan, done, nil
}
