package main

import (
	"fmt"
	"log"
	"os"

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

type Args struct {
	Service              bool           `arg:"--service" help:"start service"`
	ReadState            bool           `arg:"--read-state" help:"read the state of the network"`
	ReconfigureWifi      bool           `arg:"--reconfigure-wifi" help:"reconfigure the wifi network"`
	AddNetwork           *AddNetwork    `arg:"subcommand:add-network" help:"add a network"`
	RemoveNetwork        *RemoveNetwork `arg:"subcommand:remove-network" help:"remove a network"`
	EnableWifi           *EnableWifi    `arg:"--enable-wifi" help:"enable wifi"`
	EnableHotspot        *EnableHotspot `arg:"--enable-hotspot" help:"enable hotspot"`
	ShowNetworks         bool           `arg:"--show-networks" help:"show available networks"`                        //TODO
	IsWifiConnected      bool           `arg:"--is-wifi-connected" help:"check if wifi is connected"`                 //TODO
	ShowConnectedDevices bool           `arg:"--show-connected-devices" help:"show connected devices on the hotspot"` //TODO
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

	if args.Service {
		return startService()
	} else if args.ReadState {
		return readState()
	} else if args.ReconfigureWifi {
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
	log.Println("Running service.")
	//TODO
	return nil
}

func readState() error {
	log.Println("Reading state.")
	//TODO
	return nil
}

func reconfigureWifi() error {
	log.Println("Reconfiguring wifi.")
	//TODO
	return nil
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

	//TODO
	return nil
}

func enableHotspot(args Args) error {
	log.Println("Enabling hotspot.")
	//TODO
	return nil
}
