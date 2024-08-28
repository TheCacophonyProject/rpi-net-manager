package main

import (
	"errors"
	"runtime"
	"strings"
	"time"

	"github.com/TheCacophonyProject/rpi-net-manager/netmanagerclient"
	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
)

type service struct {
	nsm *networkStateMachine
}

func startDBusService(nsm *networkStateMachine) error {
	log.Println("Starting RPiNetManager service")
	conn, err := dbus.SystemBus()
	if err != nil {
		return err
	}
	reply, err := conn.RequestName(netmanagerclient.DbusInterface, dbus.NameFlagDoNotQueue)
	if err != nil {
		return err
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		return errors.New("name already taken")
	}

	s := &service{
		nsm: nsm,
	}
	if err := conn.Export(s, netmanagerclient.DbusPath, netmanagerclient.DbusInterface); err != nil {
		return err
	}
	if err := conn.Export(genIntrospectable(s), netmanagerclient.DbusPath, "org.freedesktop.DBus.Introspectable"); err != nil {
		return err
	}
	log.Println("Started RPiNetManager service")
	return nil
}

func genIntrospectable(v interface{}) introspect.Introspectable {
	node := &introspect.Node{
		Interfaces: []introspect.Interface{{
			Name:    netmanagerclient.DbusInterface,
			Methods: introspect.Methods(v),
		}},
	}
	return introspect.NewIntrospectable(node)
}

func sendNewNetworkState(state netmanagerclient.NetworkState) error {
	return sendBroadcast("NewNetworkState", []interface{}{string(state)})
}

func sendBroadcast(signal string, payload []interface{}) error {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Emit(dbus.ObjectPath(netmanagerclient.DbusPath), netmanagerclient.DbusInterface+"."+signal, payload...)
}

func (s service) ReadState() (string, *dbus.Error) {
	return string(s.nsm.state), nil
}

func (s service) EnableWifi(force bool) *dbus.Error {
	s.nsm.mux.Lock()
	defer s.nsm.mux.Unlock()
	s.nsm.hotspotFallback = true
	runFuncLogErr(s.nsm.setupWifi)
	return nil
}

func (s service) EnableHotspot(force bool) *dbus.Error {
	s.nsm.mux.Lock()
	defer s.nsm.mux.Unlock()
	runFuncLogErr(s.nsm.setupHotspot)
	return nil
}

func (s service) KeepHotspotOnFor(seconds int) *dbus.Error {
	s.nsm.mux.Lock()
	defer s.nsm.mux.Unlock()
	if s.nsm.state != netmanagerclient.NS_HOTSPOT_RUNNING && s.nsm.state != netmanagerclient.NS_HOTSPOT_STARTING {
		return dbusErr(errors.New("hotspot is not enabled"))
	}
	s.nsm.keepHotspotOnFor(time.Duration(seconds) * time.Second)
	return nil
}

func (s service) CheckState() *dbus.Error {
	_, _, _ = detectState()
	return nil
}

func runFuncLogErr(f func() error) {
	if err := f(); err != nil {
		log.Println("Error: ", err)
	}
}

func dbusErr(err error) *dbus.Error {
	if err == nil {
		return nil
	}
	return &dbus.Error{
		Name: netmanagerclient.DbusInterface + "." + getCallerName(),
		Body: []interface{}{err.Error()},
	}
}

func getCallerName() string {
	fpcs := make([]uintptr, 1)
	n := runtime.Callers(3, fpcs)
	if n == 0 {
		return ""
	}
	caller := runtime.FuncForPC(fpcs[0] - 1)
	if caller == nil {
		return ""
	}
	funcNames := strings.Split(caller.Name(), ".")
	return funcNames[len(funcNames)-1]
}
