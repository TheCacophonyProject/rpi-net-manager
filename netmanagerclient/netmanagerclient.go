package managementdclient

import (
	"errors"

	"github.com/godbus/dbus"
)

func SetNetworkState(state string) error {
	_, err := eventsDbusCall("org.cacophony.managementd.SetNetworkState", state)
	return err
}

func GetNetworkState() (string, error) {
	data, err := eventsDbusCall("org.cacophony.managementd.GetNetworkState")
	if err != nil {
		return "", err
	}
	if len(data) != 1 {
		return "", errors.New("error getting state")
	}
	state, ok := data[0].(string)
	if !ok {
		return "", errors.New("error reading state")
	}
	return state, nil
}

func eventsDbusCall(method string, params ...interface{}) ([]interface{}, error) {
	conn, err := dbus.SystemBus()
	if err != nil {
		return nil, err
	}
	obj := conn.Object("org.cacophony.managementd", "/org/cacophony/managementd")
	call := obj.Call(method, 0, params...)
	return call.Body, call.Err
}
