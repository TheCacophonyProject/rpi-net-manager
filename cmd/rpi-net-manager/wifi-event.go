package main

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/TheCacophonyProject/event-reporter/v3/eventclient"
)

type wifiDetails struct {
	SSID     string
	BSSID    string
	Signal   int // Signal quality as a percentage, as reported by NetworkManager.
	Freq     string
	Channel  int
	Rate     string
	Security string
}

// sendWifiConnectedEvent gathers details about the current wifi connection and
// sends a wifiConnected event. Any details that can't be found are left out.
func sendWifiConnectedEvent(conName string, prevState string, connectingDuration time.Duration) {
	details := map[string]any{
		"connectionName": conName,
		"previousState":  prevState,
	}
	if connectingDuration > 0 {
		details["connectingSeconds"] = int(connectingDuration.Seconds())
	}

	if wifi, err := getWifiDetails(); err != nil {
		log.Printf("failed to get wifi details: %v", err)
	} else {
		details["ssid"] = wifi.SSID
		details["bssid"] = wifi.BSSID
		details["signalPercent"] = wifi.Signal
		details["frequency"] = wifi.Freq
		details["channel"] = wifi.Channel
		details["bitrate"] = wifi.Rate
		details["security"] = wifi.Security
	}

	if ip, err := getWifiIPAddress(); err != nil {
		log.Printf("failed to get wifi IP address: %v", err)
	} else {
		details["ipAddress"] = ip
	}

	log.Printf("Making wifiConnected event: %v", details)
	err := eventclient.AddEvent(eventclient.Event{
		Timestamp: time.Now(),
		Type:      "wifiConnected",
		Details:   details,
	})
	if err != nil {
		log.Printf("failed to make wifiConnected event: %v", err)
	}
}

func getWifiDetails() (*wifiDetails, error) {
	out, err := exec.Command("nmcli", "--terse", "--fields", "IN-USE,SSID,BSSID,SIGNAL,FREQ,CHAN,RATE,SECURITY",
		"device", "wifi", "list", "ifname", "wlan0", "--rescan", "no").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to run nmcli: %v, output: %s", err, out)
	}
	return parseWifiDetails(string(out))
}

// parseWifiDetails finds the in-use access point in the terse output of
// 'nmcli --fields IN-USE,SSID,BSSID,SIGNAL,FREQ,CHAN,RATE,SECURITY device wifi list'.
func parseWifiDetails(output string) (*wifiDetails, error) {
	for line := range strings.SplitSeq(output, "\n") {
		fields := splitTerseLine(line)
		if len(fields) != 8 || fields[0] != "*" {
			// Lines not starting with '*' are not in-use access points
			continue
		}
		signal, err := strconv.Atoi(fields[3])
		if err != nil {
			return nil, fmt.Errorf("failed to parse signal '%s': %v", fields[3], err)
		}
		channel, err := strconv.Atoi(fields[5])
		if err != nil {
			return nil, fmt.Errorf("failed to parse channel '%s': %v", fields[5], err)
		}
		return &wifiDetails{
			SSID:     fields[1],
			BSSID:    fields[2],
			Signal:   signal,
			Freq:     fields[4],
			Channel:  channel,
			Rate:     fields[6],
			Security: fields[7],
		}, nil
	}
	return nil, fmt.Errorf("no in-use access point found")
}

// splitTerseLine splits a line of nmcli terse output on ':', handling the
// '\:' and '\\' escapes that nmcli uses within values.
func splitTerseLine(line string) []string {
	fields := []string{}
	var field strings.Builder
	escaped := false
	for _, r := range line {
		switch {
		case escaped:
			field.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		case r == ':':
			fields = append(fields, field.String())
			field.Reset()
		default:
			field.WriteRune(r)
		}
	}
	return append(fields, field.String())
}

func getWifiIPAddress() (string, error) {
	out, err := exec.Command("nmcli", "--terse", "--fields", "IP4.ADDRESS", "device", "show", "wlan0").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to run nmcli: %v, output: %s", err, out)
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if after, ok := strings.CutPrefix(line, "IP4.ADDRESS[1]:"); ok {
			return strings.TrimSpace(after), nil
		}
	}
	return "", fmt.Errorf("no IP address found")
}
