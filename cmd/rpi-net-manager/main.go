package main

import (
	"fmt"
	"log"
	"os"
	"time"

	netmanagerclient "github.com/TheCacophonyProject/rpi-net-manager/netmanagerclient"
	"github.com/alexflint/go-arg"
)

var version = "<not set>"

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
	ReconfigureWifi      *subcommand    `arg:"subcommand:reconfigure-wifi" help:"reconfigure the wifi network"`
	SavedWifiNetworks    *subcommand    `arg:"subcommand:saved-wifi-networks" help:"show saved wifi networks"`
	AddWifiNetwork       *AddNetwork    `arg:"subcommand:add-wifi-network" help:"add a network"`
	RemoveWifiNetwork    *RemoveNetwork `arg:"subcommand:remove-wifi-network" help:"remove a network"`
	EnableWifi           *EnableWifi    `arg:"subcommand:enable-wifi" help:"enable wifi"`
	EnableHotspot        *EnableHotspot `arg:"subcommand:enable-hotspot" help:"enable hotspot"`
	ScanNetwork          *subcommand    `arg:"subcommand:scan-network" help:"show available networks"`
	ShowConnectedDevices *subcommand    `arg:"subcommand:show-connected-devices" help:"show connected devices on the hotspot //TODO"` //TODO
	ModemStatus          *subcommand    `arg:"subcommand:modem-status" help:"show modem status //TODO"`                               //TODO
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
	log.SetFlags(0)

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
	} else if args.ReconfigureWifi != nil {
		return reconfigureWifi()
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
	} else {
		return fmt.Errorf("no command given, use --help for usage")
	}
}

func startService() error {
	nh := &networkHandler{}
	nh.keepHotspotOnTimer = *time.NewTimer(0)
	nh.keepHotspotOnUntil = time.Now()
	if err := startDBusService(nh); err != nil {
		return err
	}
	hotspotUsageTimeout := 1 * time.Minute
	log.Println("Setting up wifi.")
	if err := nh.setupWifi(); err != nil {
		log.Println("Failed to setup wifi:", err)
		return nil
	}

	log.Println("Checking if device is connected to a network.")
	connected, err := waitAndCheckIfConnectedToNetwork()
	if err != nil {
		log.Println("Error checking if device connected to network:", err)
		return nil
	}
	if connected {
		log.Println("Connected to network. Not starting up hotspot.")
		return nil
	}

	log.Println("Starting hotspot")
	if err := nh.setupHotspot(); err != nil {
		log.Println("Failed to setup hotspot:", err)
		return nil
	}
	nh.keepHotspotOnFor(hotspotUsageTimeout)
	//hotspotUsageTimer := time.NewTimer(hotspotUsageTimeout)
	<-nh.keepHotspotOnTimer.C // Hotspot has not been used for a while, stop it.
	log.Println("Hotspot timer expired, stopping hotspot and starting wifi.")
	if err := nh.setupWifi(); err != nil {
		log.Println("Failed to stop hotspot:", err)
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

func reconfigureWifi() error {
	log.Println("Reconfiguring wifi.")
	return netmanagerclient.ReconfigureWifi()
}

func savedWifiNetworks() error {
	log.Println("Listing saved wifi networks.")
	networks, err := netmanagerclient.ListSavedWifiNetworks()
	if err != nil {
		return err
	}
	for _, network := range networks {
		log.Println(network)
	}
	return nil
}

func addWifiNetwork(ssid, pass string) error {
	log.Println("Adding network. SSID: ", ssid, " Pass: ", pass)
	return netmanagerclient.AddWifiNetwork(ssid, pass)
}

func removeWifiNetwork(ssid string) error {
	log.Println("Removing network. SSID: ", ssid)
	return netmanagerclient.RemoveWifiNetwork(ssid)
}

func enableWifi(args Args) error {
	log.Println("Enabling wifi.")
	return netmanagerclient.EnableWifi(args.EnableWifi.Force)
}

func (nh *networkHandler) enableWifi(force bool) error {
	return nil
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
		log.Printf("SSID: '%s', Quality: '%s'", network.SSID, network.Quality)
	}
	return nil
}
