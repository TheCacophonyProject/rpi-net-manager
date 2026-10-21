package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseWifiDetails(t *testing.T) {
	output := ` :bushnet2:7C\:FF\:4D\:1B\:58\:3E:100:2462 MHz:11:195 Mbit/s:WPA2
*:my\:wifi:7C\:FF\:4D\:1B\:58\:3F:97:5660 MHz:132:405 Mbit/s:WPA2 WPA3
 :OTBC:70\:A7\:41\:DC\:5D\:21:79:2437 MHz:6:130 Mbit/s:WPA2
`
	wifi, err := parseWifiDetails(output)
	require.NoError(t, err)
	assert.Equal(t, &wifiDetails{
		SSID:     "my:wifi",
		BSSID:    "7C:FF:4D:1B:58:3F",
		Signal:   97,
		Freq:     "5660 MHz",
		Channel:  132,
		Rate:     "405 Mbit/s",
		Security: "WPA2 WPA3",
	}, wifi)
}

func TestParseWifiDetailsNotConnected(t *testing.T) {
	output := ` :bushnet2:7C\:FF\:4D\:1B\:58\:3E:100:2462 MHz:11:195 Mbit/s:WPA2
`
	_, err := parseWifiDetails(output)
	assert.Error(t, err)
}
