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
	Service              *subcommand    `arg:"subcommand" help:"start service"`
	ReadState            *ReadState     `arg:"subcommand:read-state" help:"read the state of the network"`
	ReconfigureWifi      *subcommand    `arg:"subcommand" help:"reconfigure the wifi network"`
	AddNetwork           *AddNetwork    `arg:"subcommand" help:"add a network"`
	RemoveNetwork        *RemoveNetwork `arg:"subcommand" help:"remove a network"`
	EnableWifi           *EnableWifi    `arg:"subcommand" help:"enable wifi"`
	EnableHotspot        *EnableHotspot `arg:"subcommand" help:"enable hotspot"`
	ShowNetworks         *subcommand    `arg:"subcommand" help:"show available networks"`               //TODO
	ShowConnectedDevices *subcommand    `arg:"subcommand" help:"show connected devices on the hotspot"` //TODO
	ModemStatus          *subcommand    `arg:"subcommand" help:"show modem status"`                     //TODO
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
	} else if args.AddNetwork != nil {
		return addNetwork(args.AddNetwork.SSID, args.AddNetwork.Pass)
	} else if args.RemoveNetwork != nil {
		return removeNetwork(args.RemoveNetwork.SSID)
	} else if args.EnableWifi != nil {
		return enableWifi(args)
	} else if args.EnableHotspot != nil {
		return enableHotspot(args)
	} else {
		return fmt.Errorf("no command given, use --help for usage")
	}
}

func startService() error {
	nh := &networkHandler{}
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
	hotspotUsageTimer := time.NewTimer(hotspotUsageTimeout)
	<-hotspotUsageTimer.C // Hotspot has not been used for a while, stop it.
	log.Printf("No API usage for %s, stopping hotspot.", hotspotUsageTimeout)
	if err := nh.setupWifi(); err != nil { // Setting up
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

func addNetwork(ssid, pass string) error {
	log.Println("Adding network. SSID: ", ssid, " Pass: ", pass)
	//TODO
	return nil
}

func removeNetwork(ssid string) error {
	log.Println("Removing network. SSID: ", ssid)
	//TODO
	return nil
}

func enableWifi(args Args) error {
	log.Println("Enabling wifi.")
	return netmanagerclient.EnableWifi(args.EnableWifi.Force)
}

func enableHotspot(args Args) error {
	log.Println("Enabling hotspot.")
	return netmanagerclient.EnableHotspot(args.EnableHotspot.Force)
}
